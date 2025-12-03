// main.go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	storageDir      = "./storage"       // where each video's HLS output will live
	maxUploadSize   = 1 << 30           // 1GB (adjust as needed)
	ffmpegTimeout   = 10 * time.Minute  // how long we allow ffmpeg to run
	basicAuthUser   = "admin"           // simple demo auth user
	basicAuthPass   = "secret"          // demo auth password (change)
	hlsSegmentTime  = "4"               // seconds per HLS segment
)

func main() {
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		log.Fatalf("create storage dir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", basicAuth(uploadHandler))
	// Serve the HLS files (index.m3u8 + .ts) with caching headers
	mux.Handle("/hls/", http.StripPrefix("/hls/", http.HandlerFunc(hlsHandler)))

	addr := ":8080"
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, cors(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// cors wraps responses to allow cross-origin requests (useful for browser playback during dev)

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // tighten in prod
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// basicAuth is a tiny middleware for demo HTTP Basic Auth

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != basicAuthUser || pass != basicAuthPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// uploadHandler accepts multipart/form-data with a file field named "file".
// It saves to a temp file, runs ffmpeg to convert to HLS and returns the HLS URL.

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// limit request size to prevent resource exhaustion
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "could not parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field 'file' required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// simple validation: allow mp4 and mov and mkv
	if !isAllowedExt(fh.Filename) {
		http.Error(w, "only mp4/mov/mkv allowed", http.StatusBadRequest)
		return
	}

	videoID := randomID(12)
	outDir := filepath.Join(storageDir, videoID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		http.Error(w, "internal mkdir error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// save uploaded file to tmp path
	tempPath := filepath.Join(outDir, "upload"+filepath.Ext(fh.Filename))
	if err := saveUploadedFile(file, tempPath); err != nil {
		http.Error(w, "save error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// convert to HLS via ffmpeg
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()

	if err := convertToHLS(ctx, tempPath, outDir); err != nil {
		log.Printf("ffmpeg error: %v", err)
		http.Error(w, "transcode error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// optionally remove uploaded file to save space
	_ = os.Remove(tempPath)

	hlsURL := fmt.Sprintf("/hls/%s/index.m3u8", videoID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"id":"%s","hls_url":"%s"}`, videoID, hlsURL)
}

// hlsHandler serves files from storage dir with Cache-Control for CDNs
func hlsHandler(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Path)
	if strings.Contains(path, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	fsPath := filepath.Join(storageDir, path)
	// set caching headers for segments and manifests
	if strings.HasSuffix(fsPath, ".ts") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if strings.HasSuffix(fsPath, ".m3u8") {
		// small TTL for playlists (so ABR updates propagate)
		w.Header().Set("Cache-Control", "public, max-age=5")
	}
	http.ServeFile(w, r, fsPath)
}

// helpers

func isAllowedExt(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".mov", ".mkv", ".webm":
		return true
	default:
		return false
	}
}

func saveUploadedFile(src multipart.File, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// convertToHLS runs ffmpeg to produce HLS segments & index.m3u8 in outDir.
// requires ffmpeg binary installed and reachable in PATH.

func convertToHLS(ctx context.Context, inputPath, outDir string) error {
	// ffmpeg args tuned for broad compatibility (VOD HLS)
	// -c:v libx264: H.264
	// -preset veryfast: faster encode (adjust for quality)
	// -crf 23: quality/size tradeoff
	// -c:a aac: audio codec
	// -hls_time: segment duration
	// -hls_segment_filename: where to write segments
	outPattern := filepath.Join(outDir, "segment_%03d.ts")
	indexPath := filepath.Join(outDir, "index.m3u8")

	args := []string{
		"-y", // overwrite
		"-i", inputPath,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-c:a", "aac",
		"-b:a", "128k",
		"-ac", "2",
		"-f", "hls",
		"-hls_time", hlsSegmentTime,
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", outPattern,
		indexPath,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// optional: log ffmpeg stderr to server logs for debugging
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg timeout reached: %w", err)
		}
		return fmt.Errorf("ffmpeg failed: %w", err)
	}
	return nil
}
