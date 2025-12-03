package main

import (
	"bytes"
	"context"
	"crypto/sha1"
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
	client  *b2.Client
	bkt     *b2.Bucket
	tpls    *template.Template
	bktName string
)

func main() {
	// 1. Load Env
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ No .env file found, using system environment variables")
	}
	appKeyID := os.Getenv("B2_KEY_ID")
	appKey := os.Getenv("B2_APP_KEY")
	bktName = os.Getenv("B2_BUCKET_NAME")

	if appKeyID == "" || appKey == "" || bktName == "" {
		log.Fatal("Set B2_KEY_ID, B2_APP_KEY, and B2_BUCKET_NAME env vars")
	}

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

	fmt.Println("🚀 Server running at :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ========== HELPER FUNCTIONS ==========

// getThumbPath converts "folder/video.mp4" -> "thumb/folder/video.jpg"
func getThumbPath(originalPath string) string {
	ext := path.Ext(originalPath)
	// Remove original extension and add .jpg (since all thumbs are JPEGs)
	nameWithoutExt := originalPath[:len(originalPath)-len(ext)]
	return path.Join("thumb", nameWithoutExt+".jpg")
}

func generateVideoThumbnail(videoPath string) ([]byte, error) {
	tmpImg, err := os.CreateTemp("", "vid-thumb-*.jpg")
	if err != nil { return nil, err }
	tmpImgName := tmpImg.Name()
	tmpImg.Close()
	defer os.Remove(tmpImgName)

	// FFmpeg: Seek to 1s, grab 1 frame
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01.000", "-vframes", "1", "-f", "image2", tmpImgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("FFmpeg failed: %s", string(out))
		return nil, err
	}

	imgData, err := os.ReadFile(tmpImgName)
	if err != nil { return nil, err }

	// Resize
	img, err := imaging.Decode(bytes.NewReader(imgData))
	if err != nil { return imgData, nil }
	resized := imaging.Resize(img, 300, 0, imaging.Lanczos) // 300px width
	
	buf := new(bytes.Buffer)
	err = imaging.Encode(buf, resized, imaging.JPEG)
	return buf.Bytes(), err
}

func hasSuffix(name string, suffixes ...string) bool {
	name = strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(name, strings.ToLower(s)) { return true }
	}
	return false
}

func detectContentType(name string) string {
	ext := filepath.Ext(name)
	ct := mime.TypeByExtension(ext)
	if ct != "" { return ct }
	switch ext {
	case ".mp4": return "video/mp4"
	case ".mov": return "video/quicktime"
	case ".webm": return "video/webm"
	case ".mkv": return "video/x-matroska"
	default: return "application/octet-stream"
	}
}

func humanReadableSize(size int64) string {
	const ( _ = iota; KB float64 = 1 << (10 * iota); MB; GB )
	s := float64(size)
	switch {
	case s >= GB: return fmt.Sprintf("%.2f GB", s/GB)
	case s >= MB: return fmt.Sprintf("%.2f MB", s/MB)
	case s >= KB: return fmt.Sprintf("%.2f KB", s/KB)
	default: return fmt.Sprintf("%d B", size)
	}
}

// ========== INDEX HANDLER ==========
func indexHandler(w http.ResponseWriter, r *http.Request) {
	iter := bkt.List(context.Background())
	var files []map[string]any

	for iter.Next() {
		obj := iter.Object()
		name := obj.Name()

		// NEW: Hide the entire "thumb/" directory from the main list
		if strings.HasPrefix(name, "thumb/") { 
			continue 
		}

		attrs, err := obj.Attrs(context.Background())
		if err != nil { continue }

		isMedia := hasSuffix(name, ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".mov", ".mkv", ".webm")
		thumbURL := ""
		
		if isMedia {
			// URL still points to /thumb/originalName
			// The handler will figure out the mapping
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
	if err := iter.Err(); err != nil { http.Error(w, err.Error(), 500); return }
	tpls.ExecuteTemplate(w, "index.html", map[string]any{ "BucketName": bktName, "Files": files })
}

// ========== THUMB HANDLER (Logic Updated for thumb/ folder) ==========
func thumbHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Get the Original Name from URL
	// Request: /thumb/photos/vacation.jpg
	originalName := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if originalName == "" { http.NotFound(w, r); return }

	// 2. Calculate where the thumbnail *should* be in B2
	// Original: photos/vacation.jpg -> B2 Thumb: thumb/photos/vacation.jpg
	// Original: videos/trip.mp4     -> B2 Thumb: thumb/videos/trip.jpg
	thumbB2Path := getThumbPath(originalName)

	ctx := context.Background()
	thumbObj := bkt.Object(thumbB2Path)

	// 3. Check if thumbnail exists in "thumb/" folder
	if _, err := thumbObj.Attrs(ctx); err != nil {
		// --- GENERATE MISSING THUMBNAIL ---
		log.Printf("Generating missing thumbnail: %s -> %s", originalName, thumbB2Path)

		// Download Original
		originalObj := bkt.Object(originalName)
		rc := originalObj.NewReader(ctx)
		if rc == nil { http.NotFound(w, r); return }
		defer rc.Close()

		tmpOriginal, err := os.CreateTemp("", "orig-*"+filepath.Ext(originalName))
		if err != nil { http.Error(w, "temp error", 500); return }
		defer os.Remove(tmpOriginal.Name())

		if _, err := io.Copy(tmpOriginal, rc); err != nil {
			http.Error(w, "download failed", 500); return
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
			if err != nil { http.Error(w, "decode failed", 500); return }
			
			thumbImg := imaging.Resize(srcImage, 300, 0, imaging.Lanczos)
			buf := new(bytes.Buffer)
			imaging.Encode(buf, thumbImg, imaging.JPEG)
			thumbData = buf.Bytes()
		}

		// Upload to "thumb/" folder
		thumbWr := thumbObj.NewWriter(ctx)
		if _, err := thumbWr.Write(thumbData); err != nil {
			log.Println("Failed to save thumb:", err)
		}
		thumbWr.Close()

		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(thumbData)
		return
	}

	// --- SERVE EXISTING THUMBNAIL ---
	rc := thumbObj.NewReader(ctx)
	if rc == nil { http.Error(w, "failed", 500); return }
	defer rc.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	io.Copy(w, rc)
}

// ========== UPLOAD HANDLER ==========
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		tpls.ExecuteTemplate(w, "upload.html", map[string]any{ "BucketName": bktName, "Message": "" })
		return
	}

	// 1. Get File
	file, header, err := r.FormFile("file")
	if err != nil { http.Error(w, "read error", 400); return }
	defer file.Close()

	// 2. Determine Path (Folder + Custom Name)
	folder := r.FormValue("folder")
	customName := r.FormValue("custom_name")
	if customName == "" { customName = header.Filename }
	
	objectPath := customName
	if folder != "" {
		objectPath = path.Join(folder, customName)
	}

	// 3. Temp File
	tmpFile, err := os.CreateTemp("", "upload-*"+filepath.Ext(objectPath))
	if err != nil { http.Error(w, "temp error", 500); return }
	defer os.Remove(tmpFile.Name())

	hasher := sha1.New()
	size, err := io.Copy(io.MultiWriter(tmpFile, hasher), file)
	if err != nil { http.Error(w, "copy error", 500); return }
	
	log.Println("SHA1:", hex.EncodeToString(hasher.Sum(nil)))

	// 4. Upload Original
	tmpFile.Seek(0, 0)
	obj := bkt.Object(objectPath)
	wr := obj.NewWriter(context.Background())
	if _, err = io.Copy(wr, tmpFile); err != nil { http.Error(w, "upload failed", 500); return }
	wr.Close()

	// 5. Generate Thumbnail (to thumb/ folder)
	tmpFile.Close()
	
	var thumbData []byte
	var genErr error
	shouldGen := false

	if hasSuffix(objectPath, ".mp4", ".mov", ".mkv", ".webm") {
		thumbData, genErr = generateVideoThumbnail(tmpFile.Name())
		if genErr == nil { shouldGen = true }
	} else if hasSuffix(objectPath, ".jpg", ".jpeg", ".png", ".gif", ".webp") {
		f, _ := os.Open(tmpFile.Name())
		srcImage, err := imaging.Decode(f)
		f.Close()
		if err == nil {
			thumbImg := imaging.Resize(srcImage, 300, 0, imaging.Lanczos)
			buf := new(bytes.Buffer)
			imaging.Encode(buf, thumbImg, imaging.JPEG)
			thumbData = buf.Bytes()
			shouldGen = true
		}
	}

	if shouldGen {
		// Use helper to determine thumb path
		thumbName := getThumbPath(objectPath)

		thumbObj := bkt.Object(thumbName)
		thumbWr := thumbObj.NewWriter(context.Background())
		thumbWr.Write(thumbData)
		thumbWr.Close()
		log.Println("✅ Generated Thumbnail:", thumbName)
	}

	tpls.ExecuteTemplate(w, "upload.html", map[string]any{
		"BucketName": bktName,
		"Message":    fmt.Sprintf("✅ Uploaded %s (%s)", objectPath, humanReadableSize(size)),
	})
}

// ... viewHandler, viewerHandler, downloadHandler remain exactly the same ...
func viewHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/view/")
	if name == "" { http.NotFound(w, r); return }
	obj := bkt.Object(name)
	rc := obj.NewReader(context.Background())
	if rc == nil { http.Error(w, "failed", 500); return }
	defer rc.Close()
	if r.URL.Query().Get("raw") == "true" {
		w.Header().Set("Content-Type", detectContentType(name))
		io.Copy(w, rc)
		return
	}
	tmpFile, err := os.CreateTemp("", "view-*")
	if err != nil { http.Error(w, "temp error", 500); return }
	defer os.Remove(tmpFile.Name())
	io.Copy(tmpFile, rc)
	tmpFile.Seek(0, 0)
	http.ServeContent(w, r, name, time.Now(), tmpFile)
}

func viewerHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/viewer/")
	obj := bkt.Object(name)
	attrs, err := obj.Attrs(context.Background())
	if err != nil { log.Println("Error getting attrs:", err) }
	size := "Unknown size"
	if attrs != nil { size = humanReadableSize(attrs.Size) }

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
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(name))
	io.Copy(w, rc)
}
