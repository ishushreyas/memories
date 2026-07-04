package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html/template"
	"path" // Used for B2 paths (forward slashes)

	// Image decoders
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath" // Used for local OS file paths
	"strings"
	"sync" // Added for concurrent uploads

	"github.com/disintegration/imaging"
	"github.com/joho/godotenv"
	"github.com/kurin/blazer/b2"
)

var (
	client        *b2.Client
	bkt           *b2.Bucket
	tpls          *template.Template
	bktName       string
	encryptionKey []byte
	magicHeader   = []byte("ENC1")
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ No .env file found, using system environment variables")
	}
	appKeyID := os.Getenv("B2_KEY_ID")
	appKey := os.Getenv("B2_APP_KEY")
	bktName = os.Getenv("B2_BUCKET_NAME")
	encPass := os.Getenv("ENCRYPTION_PASS")

	if appKeyID == "" || appKey == "" || bktName == "" || encPass == "" {
		log.Fatal("Set B2_KEY_ID, B2_APP_KEY, B2_BUCKET_NAME, and ENCRYPTION_PASS env vars")
	}

	hash := sha256.Sum256([]byte(encPass))
	encryptionKey = hash[:]

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("❌ FFmpeg is not installed. Please install it.")
	}

	var err error
	client, err = b2.NewClient(context.Background(), appKeyID, appKey)
	if err != nil {
		log.Fatal("B2 auth error:", err)
	}
	bkt, err = client.Bucket(context.Background(), bktName)
	if err != nil {
		log.Fatal("Bucket error:", err)
	}

	tpls = template.Must(template.New("").Funcs(template.FuncMap{
		"hasPrefix": strings.HasPrefix,
		"hasSuffix": hasSuffix,
	}).ParseGlob("templates/*.html"))

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/view/", viewHandler)
	http.HandleFunc("/download/", downloadHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/thumb/", thumbHandler)
	http.HandleFunc("/convert", convertHandler)

	fmt.Println("🚀 Server running at :8080 with Concurrent HLS Uploads enabled!")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ========== ENCRYPTION HELPERS ==========

func NewEncryptReader(r io.Reader, key []byte) (io.Reader, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, iv)
	prefix := append(magicHeader, iv...)
	prefixReader := bytes.NewReader(prefix)
	cipherReader := &cipher.StreamReader{S: stream, R: r}
	return io.MultiReader(prefixReader, cipherReader), nil
}

func NewDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	header := make([]byte, len(magicHeader))
	n, err := io.ReadFull(r, header)
	if err != nil {
		return io.MultiReader(bytes.NewReader(header[:n]), r), nil
	}
	if string(header) != string(magicHeader) {
		return io.MultiReader(bytes.NewReader(header), r), nil
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(r, iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, iv)
	return &cipher.StreamReader{S: stream, R: r}, nil
}

func uploadToB2(b2Path string, data []byte) error {
	encReader, err := NewEncryptReader(bytes.NewReader(data), encryptionKey)
	if err != nil {
		return err
	}
	obj := bkt.Object(b2Path)
	wr := obj.NewWriter(context.Background())
	if _, err := io.Copy(wr, encReader); err != nil {
		wr.Close()
		return err
	}
	return wr.Close() // Return error if finalizing the upload fails
}

// ========== HELPER FUNCTIONS ==========

func getThumbPath(originalPath string) string {
	ext := path.Ext(originalPath)
	nameWithoutExt := originalPath[:len(originalPath)-len(ext)]
	return path.Join("thumb", nameWithoutExt+".jpg")
}

func generateVideoThumbnail(videoPath string) ([]byte, error) {
	tmpImg, err := os.CreateTemp("", "vid-thumb-*.jpg")
	if err != nil {
		return nil, err
	}
	tmpImgName := tmpImg.Name()
	tmpImg.Close()
	defer os.Remove(tmpImgName)

	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01.000", "-vframes", "1", "-f", "image2", tmpImgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("FFmpeg failed: %s", string(out))
	}

	imgData, err := os.ReadFile(tmpImgName)
	if err != nil {
		return nil, err
	}

	img, err := imaging.Decode(bytes.NewReader(imgData))
	if err != nil {
		return imgData, nil
	}
	resized := imaging.Resize(img, 300, 0, imaging.Lanczos)
	buf := new(bytes.Buffer)
	imaging.Encode(buf, resized, imaging.JPEG)
	return buf.Bytes(), nil
}

func hasSuffix(name string, suffixes ...string) bool {
	name = strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(name, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func detectContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".m3u8":
		return "application/x-mpegURL"
	case ".ts":
		return "video/MP2T"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	default:
		ct := mime.TypeByExtension(ext)
		if ct != "" {
			return ct
		}
		return "application/octet-stream"
	}
}

func humanReadableSize(size int64) string {
	const (_ = iota; KB float64 = 1 << (10 * iota); MB; GB)
	s := float64(size)
	switch {
	case s >= GB:
		return fmt.Sprintf("%.2f GB", s/GB)
	case s >= MB:
		return fmt.Sprintf("%.2f MB", s/MB)
	case s >= KB:
		return fmt.Sprintf("%.2f KB", s/KB)
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// ========== HLS TRANSCODER ==========

func convertToHLS(originalB2Path, localFilePath string) error {
	baseDir := path.Dir(originalB2Path)
	if baseDir == "." {
		baseDir = ""
	}
	
	baseName := path.Base(originalB2Path)
	m3u8Name := baseName + ".m3u8"
	
	hlsFolderName := strings.ReplaceAll(baseName, " ", "_") + "_HLS"
	
	b2HlsFolder := path.Join(baseDir, hlsFolderName)
	m3u8B2Path := path.Join(b2HlsFolder, m3u8Name)

	hlsDir, err := os.MkdirTemp("", "hls-out-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(hlsDir)

	isVideo := hasSuffix(originalB2Path, ".mp4", ".mov", ".mkv", ".webm")

	if isVideo {
		if thumbData, err := generateVideoThumbnail(localFilePath); err == nil {
			uploadToB2(getThumbPath(m3u8B2Path), thumbData)
		}
	}

	var cmd *exec.Cmd
	segmentPattern := "%03d.ts"

	if isVideo {
		cmd = exec.Command("ffmpeg", "-y", "-i", localFilePath,
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "26",
			"-g", "48", "-sc_threshold", "0",
			"-c:a", "aac", "-b:a", "128k",
			"-hls_time", "6", "-hls_list_size", "0",
			"-f", "hls",
			"-hls_segment_filename", segmentPattern,
			m3u8Name)
	} else {
		cmd = exec.Command("ffmpeg", "-y", "-i", localFilePath,
			"-c:a", "aac", "-b:a", "128k",
			"-hls_time", "6", "-hls_list_size", "0",
			"-f", "hls",
			"-hls_segment_filename", segmentPattern,
			m3u8Name)
	}

	cmd.Dir = hlsDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("FFmpeg failed: %v\nOutput: %s", err, stderr.String())
		return fmt.Errorf("ffmpeg error: %v", err)
	}

	// 3. CONCURRENT UPLOAD: Upload chunks concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, 1000) // Channel to capture errors
	sem := make(chan struct{}, 10)  // Semaphore to limit concurrent uploads (max 10 at a time)

	err = filepath.Walk(hlsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil { return err }
		if info.IsDir() { return nil }
		
		relPath, _ := filepath.Rel(hlsDir, p)
		b2Path := path.Join(b2HlsFolder, filepath.ToSlash(relPath))
		
		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore slot

		go func(localPath, cloudPath string) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore slot when done

			fileData, readErr := os.ReadFile(localPath)
			if readErr != nil {
				errCh <- readErr
				return
			}
			
			if upErr := uploadToB2(cloudPath, fileData); upErr != nil {
				errCh <- fmt.Errorf("failed to upload %s: %v", cloudPath, upErr)
			} else {
				log.Printf("Uploaded chunk: %s", cloudPath)
			}
		}(p, b2Path)

		return nil
	})

	if err != nil {
		return err
	}

	// Wait for all goroutines to finish
	wg.Wait()
	close(errCh)

	// If any of the concurrent uploads failed, return the first error found
	for uploadErr := range errCh {
		if uploadErr != nil {
			return uploadErr
		}
	}

	return nil
}

// ========== HANDLERS ==========

func indexHandler(w http.ResponseWriter, r *http.Request) {
	iter := bkt.List(context.Background())
	var files []map[string]any

	for iter.Next() {
		obj := iter.Object()
		name := obj.Name()

		if strings.HasPrefix(name, "thumb/") || hasSuffix(name, ".ts") {
			continue
		}

		attrs, err := obj.Attrs(context.Background())
		if err != nil {
			continue
		}
		
		displayName := name
		if strings.HasSuffix(name, ".m3u8") && strings.Contains(name, "_HLS/") {
			parts := strings.Split(name, "_HLS/")
			if len(parts) == 2 {
				displayName = parts[0] + "/" + parts[1]
				displayName = strings.Replace(displayName, ".m3u8", "", 1)
			}
		}

		isMedia := hasSuffix(name, ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".mov", ".mkv", ".webm", ".m3u8", ".mp3", ".wav", ".aac", ".m4a")
		thumbURL := "/static/file-icon.png"
		if isMedia {
			thumbURL = "/thumb/" + name
		}

		files = append(files, map[string]any{
			"Name":        name,
			"DisplayName": displayName,
			"Size":        humanReadableSize(attrs.Size),
			"Time":        attrs.UploadTimestamp.Format("02 Jan"),
			"ContentType": detectContentType(name),
			"ThumbURL":    thumbURL,
		})
	}
	tpls.ExecuteTemplate(w, "index.html", map[string]any{"BucketName": bktName, "Files": files})
}

func thumbHandler(w http.ResponseWriter, r *http.Request) {
	originalName := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if originalName == "" {
		http.NotFound(w, r)
		return
	}

	thumbObj := bkt.Object(getThumbPath(originalName))

	if _, err := thumbObj.Attrs(context.Background()); err != nil {
		http.Redirect(w, r, "/static/file-icon.png", 302)
		return
	}

	rc := thumbObj.NewReader(context.Background())
	if rc == nil {
		http.Redirect(w, r, "/static/file-icon.png", 302)
		return
	}
	defer rc.Close()

	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Redirect(w, r, "/static/file-icon.png", 302)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	io.Copy(w, decReader)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		tpls.ExecuteTemplate(w, "upload.html", map[string]any{"BucketName": bktName, "Message": ""})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "read error", 400)
		return
	}
	defer file.Close()

	folder := r.FormValue("folder")
	customName := r.FormValue("custom_name")
	if customName == "" {
		customName = header.Filename
	}

	objectPath := customName
	if folder != "" {
		objectPath = path.Join(folder, customName)
	}

	tmpFile, err := os.CreateTemp("", "upload-*"+filepath.Ext(objectPath))
	if err != nil {
		http.Error(w, "temp error", 500)
		return
	}
	defer os.Remove(tmpFile.Name())

	hasher := sha1.New()
	size, err := io.Copy(io.MultiWriter(tmpFile, hasher), file)
	if err != nil {
		http.Error(w, "copy error", 500)
		return
	}
	tmpFile.Seek(0, 0)

	isVideo := hasSuffix(objectPath, ".mp4", ".mov", ".mkv", ".webm")
	isAudio := hasSuffix(objectPath, ".mp3", ".wav", ".m4a", ".aac")

	if size > 4*1024*1024 && (isVideo || isAudio) {
		log.Println("Large media detected. Transcoding to HLS...")
		if err := convertToHLS(objectPath, tmpFile.Name()); err == nil {
			tpls.ExecuteTemplate(w, "upload.html", map[string]any{
				"BucketName": bktName,
				"Message":    fmt.Sprintf("✅ Converted to HLS & Uploaded: %s_HLS", path.Base(objectPath)),
			})
			return
		} else {
			log.Println("Conversion failed, falling back to raw upload:", err)
		}
	}

	var thumbData []byte
	shouldGenThumb := false

	if isVideo {
		thumbData, err = generateVideoThumbnail(tmpFile.Name())
		if err == nil {
			shouldGenThumb = true
		}
	} else if hasSuffix(objectPath, ".jpg", ".jpeg", ".png", ".gif", ".webp") {
		srcImage, err := imaging.Decode(tmpFile)
		if err == nil {
			thumbImg := imaging.Resize(srcImage, 300, 0, imaging.Lanczos)
			buf := new(bytes.Buffer)
			imaging.Encode(buf, thumbImg, imaging.JPEG)
			thumbData = buf.Bytes()
			shouldGenThumb = true
		}
		tmpFile.Seek(0, 0)
	}

	if shouldGenThumb {
		uploadToB2(getThumbPath(objectPath), thumbData)
	}

	fileData, _ := os.ReadFile(tmpFile.Name())
	uploadToB2(objectPath, fileData)

	tpls.ExecuteTemplate(w, "upload.html", map[string]any{
		"BucketName": bktName,
		"Message":    fmt.Sprintf("✅ Uploaded %s (%s)", objectPath, humanReadableSize(size)),
	})
}

func convertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", 400)
		return
	}

	ctx := context.Background()
	obj := bkt.Object(req.Filename)
	rc := obj.NewReader(ctx)
	if rc == nil {
		http.Error(w, "File not found", 404)
		return
	}

	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		rc.Close()
		http.Error(w, "Decryption error", 500)
		return
	}

	tmpFile, err := os.CreateTemp("", "conv-*"+filepath.Ext(req.Filename))
	if err != nil {
		rc.Close()
		http.Error(w, "Temp file error", 500)
		return
	}
	defer os.Remove(tmpFile.Name())

	io.Copy(tmpFile, decReader)
	rc.Close()
	tmpFile.Close()

	if err := convertToHLS(req.Filename, tmpFile.Name()); err != nil {
		log.Println("HLS Conversion failed:", err)
		http.Error(w, "Conversion failed", 500)
		return
	}

	baseDir := path.Dir(req.Filename)
	if baseDir == "." {
		baseDir = ""
	}
	hlsFolderName := strings.ReplaceAll(path.Base(req.Filename), " ", "_") + "_HLS"
	m3u8B2Path := path.Join(baseDir, hlsFolderName, path.Base(req.Filename)+".m3u8")

	if _, err := bkt.Object(m3u8B2Path).Attrs(ctx); err == nil {
		obj.Delete(ctx)
		bkt.Object(getThumbPath(req.Filename)).Delete(ctx)
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
	} else {
		log.Println("Safety Abort: .m3u8 not found after upload.")
		http.Error(w, "Verification failed", 500)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/view/")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	obj := bkt.Object(name)
	rc := obj.NewReader(context.Background())
	if rc == nil {
		http.Error(w, "File not found", 404)
		return
	}
	defer rc.Close()

	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Error(w, "Decryption error", 500)
		return
	}

	w.Header().Set("Content-Type", detectContentType(name))
	io.Copy(w, decReader)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/download/")

	obj := bkt.Object(name)
	rc := obj.NewReader(context.Background())
	if rc == nil {
		http.Error(w, "File not found", 404)
		return
	}
	defer rc.Close()

	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Error(w, "Decryption error", 500)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(name))
	w.Header().Set("Content-Type", detectContentType(name))
	io.Copy(w, decReader)
}