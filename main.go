
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"archive/zip"
	"io/ioutil"
)

// Config stores server configuration.
type Config struct {
	AuthToken      string `json:"auth_token"`
	DeploymentPath string `json:"deployment_path"`
	Port           string `json:"port"`
	AllowedOrigins string `json:"allowed_origins"`
	LogPath        string `json:"log_path"`
	ExportType     string `json:"export_type"`
	BaseURL        string `json:"base_url"`
	Title          string `json:"title"`
	Theme          string `json:"theme"`
}

// Response is the standard API response structure.
type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// FileInfo stores information about a processed file.
type FileInfo struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

var config Config
var logger *log.Logger

func main() {
	// Initialize logging
	setupLogging()

	// Read configuration
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		logger.Fatalf("Error reading configuration file: %v", err)
	}

	if err := json.Unmarshal(configFile, &config); err != nil {
		logger.Fatalf("Error parsing configuration: %v", err)
	}

	// Ensure deployment directory exists
	if err := os.MkdirAll(config.DeploymentPath, 0755); err != nil {
		logger.Fatalf("Error creating deployment directory: %v", err)
	}

	// Set up HTTP handlers
	http.HandleFunc("/deploy", authenticateMiddleware(handleDeploy))
	http.HandleFunc("/health", authenticateMiddleware(handleHealth))
	http.HandleFunc("/info", authenticateMiddleware(handleInfo))

	// Start server
	logger.Printf("Deployment API server running on port %s...", config.Port)
	logger.Printf("Deployment path: %s", config.DeploymentPath)
	logger.Fatal(http.ListenAndServe(":"+config.Port, nil))
}

// setupLogging configures logging to file and console.
func setupLogging() {
	// Create logs directory if it doesn't exist
	if err := os.MkdirAll("logs", 0755); err != nil {
		log.Fatalf("Error creating logs directory: %v", err)
	}

	// Open log file
	logFile, err := os.OpenFile(filepath.Join("logs", "deployment_server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Error opening log file: %v", err)
	}

	// Create multi-writer for both file and console
	logger = log.New(io.MultiWriter(logFile, os.Stdout), "[DeploymentAPI] ", log.LstdFlags)
}

// authenticateMiddleware checks the authentication token.
func authenticateMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Add CORS headers
		if config.AllowedOrigins != "" {
			w.Header().Set("Access-Control-Allow-Origin", config.AllowedOrigins)
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		authHeader := r.Header.Get("Authorization")
		
		// Header format must be "Bearer <token>"
		if !strings.HasPrefix(authHeader, "Bearer ") {
			sendResponse(w, false, "Invalid authentication", http.StatusUnauthorized)
			return
		}
		
		token := strings.TrimPrefix(authHeader, "Bearer ")
		
		// Compare token securely
		if subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken)) != 1 {
			sendResponse(w, false, "Invalid token", http.StatusUnauthorized)
			return
		}
		
		next(w, r)
	}
}

// handleHealth checks server health.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	sendResponse(w, true, "Server is running properly", http.StatusOK)
}

// handleInfo provides server information.
func handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"deployment_path": config.DeploymentPath,
		"server_time":     time.Now().Format(time.RFC3339),
		"version":         "1.0.0",
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleDeploy handles file deployment requests.
func handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendResponse(w, false, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 100MB file size limit
	r.ParseMultipartForm(100 << 20)
	
	// Get export_type parameter (use config value if not provided)
	exportType := r.FormValue("export_type")
	if exportType == "" {
		exportType = config.ExportType
		if exportType == "" {
			exportType = "hugo" // Default to Hugo if not specified
		}
	}
	
	// Get relative_path parameter (optional)
	relativePath := r.FormValue("relative_path")
	
	// Determine deployment path based on export type and relative path
	deployPath := filepath.Join(config.DeploymentPath, exportType)
	if relativePath != "" {
		// Validate relative path for security
		if strings.Contains(relativePath, "..") {
			sendResponse(w, false, "Invalid relative path", http.StatusBadRequest)
			return
		}
		deployPath = filepath.Join(deployPath, relativePath)
	}
	
	// Create directory if it doesn't exist
	if err := os.MkdirAll(deployPath, 0755); err != nil {
		logger.Printf("Error creating deployment directory: %v", err)
		sendResponse(w, false, fmt.Sprintf("Error creating deployment directory: %v", err), http.StatusInternalServerError)
		return
	}

	// Check if this is a site initialization request
	isInit := r.FormValue("init") == "true"
	if isInit {
		// Copy site template to deployment path
		templatePath := filepath.Join("site_templates", exportType)
		if err := copyDirectory(templatePath, deployPath); err != nil {
			logger.Printf("Error initializing site template: %v", err)
			sendResponse(w, false, fmt.Sprintf("Error initializing site template: %v", err), http.StatusInternalServerError)
			return
		}
		logger.Printf("Initialized %s site template at %s", exportType, deployPath)
	}
	
	// Process all files
	form := r.MultipartForm
	files := form.File["files"]
	
	if len(files) == 0 {
		sendResponse(w, false, "No files sent", http.StatusBadRequest)
		return
	}
	
	var processedFiles []FileInfo
	
	for _, fileHeader := range files {
		// Get filename and target path
		filename := filepath.Base(fileHeader.Filename)
		targetPath := filepath.Join(deployPath, filename)
		
		// Open source file
		src, err := fileHeader.Open()
		if err != nil {
			logger.Printf("Error opening file %s: %v", filename, err)
			continue
		}
		defer src.Close()
		
		// Create target file
		dst, err := os.Create(targetPath)
		if err != nil {
			logger.Printf("Error creating target file %s: %v", targetPath, err)
			continue
		}
		defer dst.Close()
		
		// Copy file contents
		if _, err = io.Copy(dst, src); err != nil {
			logger.Printf("Error copying file %s: %v", filename, err)
			continue
		}
		
		// Add to processed files list
		processedFiles = append(processedFiles, FileInfo{
			Path:        filepath.Join(relativePath, filename),
			ContentType: fileHeader.Header.Get("Content-Type"),
			Size:        fileHeader.Size,
		})
		
		logger.Printf("File saved successfully: %s (%d bytes)", targetPath, fileHeader.Size)
	}
	
	// Send response with processed files list
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	response := struct {
		Success bool       `json:"success"`
		Message string     `json:"message"`
		Files   []FileInfo `json:"files"`
	}{
		Success: true,
		Message: fmt.Sprintf("Successfully saved %d files to %s", len(processedFiles), deployPath),
		Files:   processedFiles,
	}
	
	json.NewEncoder(w).Encode(response)
}

// sendResponse sends a standard JSON response.
func sendResponse(w http.ResponseWriter, success bool, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	
	response := Response{
		Success: success,
		Message: message,
	}
	
	json.NewEncoder(w).Encode(response)
}

// copyDirectory recursively copies a directory tree.
func copyDirectory(src, dst string) error {
	// Check if source directory exists
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}

	// Create destination directory
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	// Read source directory
	entries, err := ioutil.ReadDir(src)
	if err != nil {
		return err
	}

	// Copy each entry
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			// Recursively copy subdirectory
			if err := copyDirectory(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// Copy file
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Create destination file
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Copy content
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	// Copy file mode
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}
