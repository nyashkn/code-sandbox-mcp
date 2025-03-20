package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Map to store artifact locations
var artifactsRegistry = make(map[string]string)

// Persistent directory for artifacts
var persistentArtifactsDir = filepath.Join(os.TempDir(), "persistent-code-sandbox-artifacts")

func init() {
	// Create the persistent artifacts directory if it doesn't exist
	if _, err := os.Stat(persistentArtifactsDir); os.IsNotExist(err) {
		err := os.MkdirAll(persistentArtifactsDir, 0755)
		if err != nil {
			fmt.Printf("Failed to create persistent artifacts directory: %v\n", err)
		} else {
			fmt.Printf("Created persistent artifacts directory: %s\n", persistentArtifactsDir)
		}
	}
}

// RegisterArtifact adds an artifact to the registry
func RegisterArtifact(containerID, name, path string) {
	key := fmt.Sprintf("%s/%s", containerID, name)
	artifactsRegistry[key] = path
}

// ListContainerArtifacts returns a list of artifacts for a container
func ListContainerArtifacts(ctx context.Context, prefix string) ([]mcp.Resource, error) {
	prefix = strings.TrimPrefix(prefix, "artifacts://")
	var resources []mcp.Resource

	for key, _ := range artifactsRegistry {
		if strings.HasPrefix(key, prefix) {
			parts := strings.Split(key, "/")
			if len(parts) >= 2 {
				fileName := parts[len(parts)-1]
				resources = append(resources, mcp.Resource{
					URI:         fmt.Sprintf("artifacts://%s", key),
					Name:        fileName,
					MIMEType:    guessMimeType(fileName),
					Description: fmt.Sprintf("Artifact %s from container %s", fileName, parts[0]),
				})
			}
		}
	}

	return resources, nil
}

// GetContainerArtifact retrieves an artifact by URI
func GetContainerArtifact(ctx context.Context, request mcp.ReadResourceRequest) ([]interface{}, error) {
	uriPath := strings.TrimPrefix(request.Params.URI, "artifacts://")

	path, ok := artifactsRegistry[uriPath]
	if !ok {
		return nil, fmt.Errorf("artifact not found: %s", uriPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifact: %w", err)
	}

	fileName := filepath.Base(path)
	mimeType := guessMimeType(fileName)

	return []interface{}{
		mcp.TextResourceContents{
			ResourceContents: mcp.ResourceContents{
				URI:      request.Params.URI,
				MIMEType: mimeType,
			},
			Text: string(data),
		},
	}, nil
}

// guessMimeType returns a simple MIME type based on file extension
func guessMimeType(filename string) string {
	// Very basic type detection based only on common extensions
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp":
		return "image"
	case ".pdf":
		return "pdf"
	case ".txt", ".md", ".json", ".yaml", ".yml", ".csv", ".tsv":
		return "text"
	case ".mp3", ".wav", ".ogg", ".flac":
		return "audio"
	case ".mp4", ".webm", ".avi", ".mov":
		return "video"
	default:
		return "binary"
	}
}

// CleanupArtifact removes an artifact from the registry and deletes the file
func CleanupArtifact(artifactPath string) {
	// Find and remove from registry
	var keysToRemove []string
	for key, path := range artifactsRegistry {
		if path == artifactPath {
			keysToRemove = append(keysToRemove, key)
		}
	}

	for _, key := range keysToRemove {
		delete(artifactsRegistry, key)
	}

	// Remove the file
	os.Remove(artifactPath)
}

// CollectArtifactsFromDir scans a directory for artifacts, copies them to destinations and registers them
// If targetPath is provided, artifacts will be copied there in addition to being registered in the MCP system
func CollectArtifactsFromDir(containerID, artifactsDir string, targetPath string) ([]string, error) {
	// Enhanced debugging with more visibility
	fmt.Printf("======= ARTIFACT COLLECTION DIAGNOSTICS =======\n")
	fmt.Printf("CollectArtifactsFromDir called with:\n")
	fmt.Printf("  containerID: %s\n", containerID)
	fmt.Printf("  artifactsDir: %s\n", artifactsDir)
	fmt.Printf("  targetPath: %s\n", targetPath)

	// Print current directory for debugging
	curDir, _ := os.Getwd()
	fmt.Printf("  Current working directory: %s\n", curDir)

	// Phase 1: Collect artifacts from container
	files, err := os.ReadDir(artifactsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifacts directory: %w", err)
	}

	if len(files) == 0 {
		fmt.Println("No artifacts found in container")
		return []string{}, nil
	}

	// Create container-specific directory in persistent storage
	containerDir := filepath.Join(persistentArtifactsDir, containerID)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create container directory: %w", err)
	}

	// Phase 2: Process and copy each artifact
	var artifactURIs []string
	for _, file := range files {
		if file.IsDir() {
			continue // Skip directories
		}

		fileName := file.Name()
		srcPath := filepath.Join(artifactsDir, fileName)

		// Read the file once
		srcData, err := os.ReadFile(srcPath)
		if err != nil {
			fmt.Printf("Warning: failed to read artifact %s: %v\n", fileName, err)
			continue
		}

		// Always copy to persistent storage (for registry)
		persistentPath := filepath.Join(containerDir, fileName)
		if err := os.WriteFile(persistentPath, srcData, 0644); err != nil {
			fmt.Printf("Warning: failed to write artifact to persistent storage: %v\n", err)
			continue
		}

		// Copy to target location if specified
		if targetPath != "" {
			// Print target path for debugging
			fmt.Printf("Target directory for artifacts: %s\n", targetPath)

			// Create the target directory if it doesn't exist
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				fmt.Printf("Warning: Failed to create target directory %s: %v\n", targetPath, err)
			} else {
				// Copy the file to the target directory
				destPath := filepath.Join(targetPath, fileName)
				fmt.Printf("Writing artifact to: %s\n", destPath)
				if err := os.WriteFile(destPath, srcData, 0644); err != nil {
					fmt.Printf("Warning: Failed to write artifact to target directory: %v\n", err)
				} else {
					fmt.Printf("Artifact copied to directory: %s\n", destPath)

					// Verify the file was actually written
					if _, err := os.Stat(destPath); err != nil {
						fmt.Printf("ERROR: After writing, file still not found at %s: %v\n", destPath, err)
					} else {
						// Get file info to verify permissions and size
						fileInfo, _ := os.Stat(destPath)
						fmt.Printf("File successfully verified at %s (size: %d bytes, mode: %s)\n",
							destPath, fileInfo.Size(), fileInfo.Mode())
					}
				}
			}
		}

		// Register the artifact with the persistent path
		RegisterArtifact(containerID, fileName, persistentPath)
		artifactURI := fmt.Sprintf("artifacts://%s/%s", containerID, fileName)
		artifactURIs = append(artifactURIs, artifactURI)
	}

	return artifactURIs, nil
}
