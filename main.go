package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

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
	magicHeader   = []byte("ENC1") // Used to detect if a file is encrypted
)

func main() {
	// 1. Load Env
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ No .env file found, using system environment variables")
	}
	appKeyID := os.Getenv("B2_KEY_ID")
	appKey := os.Getenv("B2_APP_KEY")
	bktName = os.Getenv("B2_BUCKET_NAME")
	encPass := os.Getenv("ENCRYPTION_PASS") // New variable for encryption

	if appKeyID == "" || appKey == "" || bktName == "" || encPass == "" {
		log.Fatal("Set B2_KEY_ID, B2_APP_KEY, B2_BUCKET_NAME, and ENCRYPTION_PASS env vars")
	}

	// Hash the password to generate a secure 32-byte AES-256 key
	hash := sha256.Sum256([]byte(encPass))
	encryptionKey = hash[:]

	// 2. Check for FFmpeg
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("❌ FFmpeg is not installed. Please install it to generate video thumbnails.")
	}

	// 3. Connect to B2
	var err error
	client, err = b2.NewClient(context.Background(), appKeyID, appKey)
	if err != nil {
		log.Fatal("B2 auth error:", err)
	}

	bkt, err = client.Bucket(context.Background(), bktName)
	if err != nil {
		log.Fatal("Bucket error:", err)
	}

	// 4. Templates & Routes
	tpls = template.Must(template.New("").Funcs(template.FuncMap{
		"hasPrefix": strings.HasPrefix,
		"hasSuffix": hasSuffix,
	}).ParseGlob("templates/*.html"))

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/view/", viewHandler)
	http.HandleFunc("/viewer/", viewerHandler)
	http.HandleFunc("/download/", downloadHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/thumb/", thumbHandler)

	fmt.Println("🚀 Server running at :8080 with AES-256 Encryption enabled!")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ========== ENCRYPTION HELPERS ==========

// NewEncryptReader takes a plaintext reader and outputs an encrypted stream (MagicHeader + IV + Ciphertext)
func NewEncryptReader(r io.Reader, key []byte) (io.Reader, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// Generate a 16-byte random IV
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}

	stream := cipher.NewCTR(block, iv)

	// Combine Magic Header ("ENC1") and the IV
	prefix := append(magicHeader, iv...)
	prefixReader := bytes.NewReader(prefix)
	cipherReader := &cipher.StreamReader{S: stream, R: r}

	// Output prefix first, then the encrypted data
	return io.MultiReader(prefixReader, cipherReader), nil
}

// NewDecryptReader detects if a stream is encrypted. If yes, it decrypts it. If no, it streams it back normally.
func NewDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	// Read the first 4 bytes to check for our magic header
	header := make([]byte, len(magicHeader))
	n, err := io.ReadFull(r, header)
	if err != nil {
		// File is smaller than 4 bytes, so it's not encrypted. Yield bytes normally.
		return io.MultiReader(bytes.NewReader(header[:n]), r), nil
	}

	if string(header) != string(magicHeader) {
		// No magic header found, this is an old unencrypted file. Yield bytes normally.
		return io.MultiReader(bytes.NewReader(header), r), nil
	}

	// It is encrypted. Extract the 16-byte IV.
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
		log.Printf("FFmpeg failed: %s", string(out))
		return nil, err
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
	err = imaging.Encode(buf, resized, imaging.JPEG)
	return buf.Bytes(), err
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
	ext := filepath.Ext(name)
	ct := mime.TypeByExtension(ext)
	if ct != "" {
		return ct
	}
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	default:
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

// ========== INDEX HANDLER ==========
func indexHandler(w http.ResponseWriter, r *http.Request) {
	iter := bkt.List(context.Background())
	var files []map[string]any

	for iter.Next() {
		obj := iter.Object()
		name := obj.Name()

		if strings.HasPrefix(name, "thumb/") {
			continue
		}

		attrs, err := obj.Attrs(context.Background())
		if err != nil {
			continue
		}

		isMedia := hasSuffix(name, ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".mov", ".mkv", ".webm")
		thumbURL := ""

		if isMedia {
			thumbURL = "/thumb/" + name
		} else {
			thumbURL = "/static/file-icon.png"
		}

		files = append(files, map[string]any{
			"Name":        name,
			"Size":        humanReadableSize(attrs.Size),
			"Time":        attrs.UploadTimestamp.Format("02 Jan"),
			"ContentType": detectContentType(name),
			"ThumbURL":    thumbURL,
		})
	}
	if err := iter.Err(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tpls.ExecuteTemplate(w, "index.html", map[string]any{"BucketName": bktName, "Files": files})
}

// ========== THUMB HANDLER ==========
func thumbHandler(w http.ResponseWriter, r *http.Request) {
	originalName := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if originalName == "" {
		http.NotFound(w, r)
		return
	}

	thumbB2Path := getThumbPath(originalName)
	ctx := context.Background()
	thumbObj := bkt.Object(thumbB2Path)

	if _, err := thumbObj.Attrs(ctx); err != nil {
		// --- GENERATE MISSING THUMBNAIL ---
		originalObj := bkt.Object(originalName)
		rc := originalObj.NewReader(ctx)
		if rc == nil {
			http.NotFound(w, r)
			return
		}
		defer rc.Close()

		// Decrypt original file stream to a temporary local file
		decReader, err := NewDecryptReader(rc, encryptionKey)
		if err != nil {
			http.Error(w, "decryption error", 500)
			return
		}

		tmpOriginal, err := os.CreateTemp("", "orig-*"+filepath.Ext(originalName))
		if err != nil {
			http.Error(w, "temp error", 500)
			return
		}
		defer os.Remove(tmpOriginal.Name())

		if _, err := io.Copy(tmpOriginal, decReader); err != nil {
			http.Error(w, "download/decrypt failed", 500)
			return
		}
		tmpOriginal.Close()

		var thumbData []byte
		if hasSuffix(originalName, ".mp4", ".mov", ".mkv", ".webm") {
			thumbData, err = generateVideoThumbnail(tmpOriginal.Name())
			if err != nil {
				log.Println("Video thumb failed:", err)
				http.Redirect(w, r, "/static/file-icon.png", 302)
				return
			}
		} else {
			f, _ := os.Open(tmpOriginal.Name())
			srcImage, err := imaging.Decode(f)
			f.Close()
			if err != nil {
				http.Error(w, "decode failed", 500)
				return
			}

			thumbImg := imaging.Resize(srcImage, 300, 0, imaging.Lanczos)
			buf := new(bytes.Buffer)
			imaging.Encode(buf, thumbImg, imaging.JPEG)
			thumbData = buf.Bytes()
		}

		// Upload encrypted thumbnail to B2
		thumbWr := thumbObj.NewWriter(ctx)
		encThumbReader, err := NewEncryptReader(bytes.NewReader(thumbData), encryptionKey)
		if err == nil {
			if _, err := io.Copy(thumbWr, encThumbReader); err != nil {
				log.Println("Failed to upload encrypted thumb:", err)
			}
		}
		thumbWr.Close()

		// Serve the plaintext thumbnail to the viewer
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(thumbData)
		return
	}

	// --- SERVE EXISTING THUMBNAIL ---
	rc := thumbObj.NewReader(ctx)
	if rc == nil {
		http.Error(w, "failed", 500)
		return
	}
	defer rc.Close()

	// Decrypt the thumbnail on the fly as it is served
	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Error(w, "decryption error", 500)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	io.Copy(w, decReader)
}

// ========== UPLOAD HANDLER ==========
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

	// Save plaintext temporarily so we can hash it and generate thumbnails
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

	log.Println("SHA1:", hex.EncodeToString(hasher.Sum(nil)))

	// Upload Original (Encrypted on the fly)
	tmpFile.Seek(0, 0)
	encReader, err := NewEncryptReader(tmpFile, encryptionKey)
	if err != nil {
		http.Error(w, "encryption error", 500)
		return
	}

	obj := bkt.Object(objectPath)
	wr := obj.NewWriter(context.Background())
	if _, err = io.Copy(wr, encReader); err != nil {
		http.Error(w, "upload failed", 500)
		return
	}
	wr.Close()

	// Generate and upload thumbnail
	var thumbData []byte
	var genErr error
	shouldGen := false

	if hasSuffix(objectPath, ".mp4", ".mov", ".mkv", ".webm") {
		thumbData, genErr = generateVideoThumbnail(tmpFile.Name())
		if genErr == nil {
			shouldGen = true
		}
	} else if hasSuffix(objectPath, ".jpg", ".jpeg", ".png", ".gif", ".webp") {
		tmpFile.Seek(0, 0)
		srcImage, err := imaging.Decode(tmpFile)
		if err == nil {
			thumbImg := imaging.Resize(srcImage, 300, 0, imaging.Lanczos)
			buf := new(bytes.Buffer)
			imaging.Encode(buf, thumbImg, imaging.JPEG)
			thumbData = buf.Bytes()
			shouldGen = true
		}
	}

	tmpFile.Close()

	if shouldGen {
		thumbName := getThumbPath(objectPath)
		thumbObj := bkt.Object(thumbName)
		thumbWr := thumbObj.NewWriter(context.Background())

		// Encrypt the thumbnail data on the fly
		encThumbReader, err := NewEncryptReader(bytes.NewReader(thumbData), encryptionKey)
		if err == nil {
			io.Copy(thumbWr, encThumbReader)
		}
		thumbWr.Close()
		log.Println("✅ Generated & Encrypted Thumbnail:", thumbName)
	}

	tpls.ExecuteTemplate(w, "upload.html", map[string]any{
		"BucketName": bktName,
		"Message":    fmt.Sprintf("✅ Uploaded %s (%s)", objectPath, humanReadableSize(size)),
	})
}

// ========== VIEW & DOWNLOAD HANDLERS ==========
func viewHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/view/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	obj := bkt.Object(name)
	rc := obj.NewReader(context.Background())
	if rc == nil {
		http.Error(w, "failed", 500)
		return
	}
	defer rc.Close()

	// Wrap reader to decrypt on the fly
	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Error(w, "decryption error", 500)
		return
	}

	if r.URL.Query().Get("raw") == "true" {
		w.Header().Set("Content-Type", detectContentType(name))
		io.Copy(w, decReader)
		return
	}

	tmpFile, err := os.CreateTemp("", "view-*")
	if err != nil {
		http.Error(w, "temp error", 500)
		return
	}
	defer os.Remove(tmpFile.Name())
	
	io.Copy(tmpFile, decReader) // Write decrypted payload locally
	tmpFile.Seek(0, 0)
	http.ServeContent(w, r, name, time.Now(), tmpFile)
}

func viewerHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/viewer/")
	obj := bkt.Object(name)
	attrs, err := obj.Attrs(context.Background())
	if err != nil {
		log.Println("Error getting attrs:", err)
	}
	size := "Unknown size"
	if attrs != nil {
		size = humanReadableSize(attrs.Size)
	}

	data := map[string]any{
		"FileName":    name,
		"FileSize":    size,
		"ContentType": detectContentType(name),
		"IsImage":     hasSuffix(name, ".jpg", ".jpeg", ".png", ".gif", ".webp"),
		"IsVideo":     hasSuffix(name, ".mp4", ".mov", ".mkv", ".webm"),
		"IsPDF":       hasSuffix(name, ".pdf"),
	}
	tpls.ExecuteTemplate(w, "view.html", data)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/download/")
	obj := bkt.Object(name)
	rc := obj.NewReader(context.Background())
	defer rc.Close()

	decReader, err := NewDecryptReader(rc, encryptionKey)
	if err != nil {
		http.Error(w, "decryption error", 500)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(name))
	io.Copy(w, decReader) // Stream decrypted bytes to user
}