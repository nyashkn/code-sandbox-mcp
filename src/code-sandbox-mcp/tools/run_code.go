package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automata-Labs-team/code-sandbox-mcp/languages"
	resources "github.com/Automata-Labs-team/code-sandbox-mcp/resources"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/moby/moby/client"
	"github.com/moby/moby/pkg/stdcopy"
)

func RunCodeSandbox(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	arguments := request.Params.Arguments
	steps, _ := arguments["steps"].(float64)
	if steps == 0 {
		steps = 100
	}
	server := server.ServerFromContext(ctx)
	var progressToken mcp.ProgressToken
	if request.Params.Meta != nil && request.Params.Meta.ProgressToken != nil {
		progressToken = request.Params.Meta.ProgressToken
	}

	language, ok := request.Params.Arguments["language"].(string)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("Language not supported: %s", request.Params.Arguments["language"])), nil
	}
	code, ok := request.Params.Arguments["code"].(string)
	if !ok {
		return mcp.NewToolResultError("language must be a string"), nil
	}

	// Extract output path if provided
	outputPath, _ := request.Params.Arguments["outputPath"].(string)
	// Validate that the output path exists if provided
	if outputPath != "" {
		if _, err := os.Stat(outputPath); os.IsNotExist(err) {
			// Create the directory if it doesn't exist
			if err := os.MkdirAll(outputPath, 0755); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Failed to create output directory: %v", err)), nil
			}
			fmt.Printf("Created output directory: %s\n", outputPath)
		} else if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Error checking output directory: %v", err)), nil
		}
	}
	parsed := languages.Language(language)
	config := languages.SupportedLanguages[languages.Language(language)]

	if progressToken != "" {
		if err := server.SendNotificationToClient(
			"notifications/progress",
			map[string]interface{}{
				"progress":      int(10),
				"total":         int(steps),
				"progressToken": progressToken,
			},
		); err != nil {
			return &mcp.CallToolResult{
				Content: []interface{}{
					mcp.NewTextContent("Could not send progress to client"),
				},
				IsError: false,
			}, nil
		}
	}

	cmd := config.RunCommand
	escapedCode := strings.ToValidUTF8(code, "")

	// Create a channel to receive the result from runInDocker
	resultCh := make(chan struct {
		logs      string
		artifacts []string
		err       error
	}, 1)

	// Run the Docker container in a goroutine
	go func() {
		logs, artifacts, err := runInDocker(ctx, cmd, config.Image, escapedCode, parsed, outputPath)
		resultCh <- struct {
			logs      string
			artifacts []string
			err       error
		}{logs, artifacts, err}
	}()

	progress := 20
	for {
		select {
		case result := <-resultCh:
			if progressToken != "" {
				// Send final progress update
				_ = server.SendNotificationToClient(
					"notifications/progress",
					map[string]interface{}{
						"progress":      100,
						"total":         int(steps),
						"progressToken": progressToken,
					},
				)
			}
			if result.err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Error: %v", result.err)), nil
			}

			if len(result.artifacts) > 0 {
				return mcp.NewToolResultText(fmt.Sprintf("Logs: %s\n\nArtifacts: %s", result.logs, strings.Join(result.artifacts, ", "))), nil
			} else {
				return mcp.NewToolResultText(fmt.Sprintf("Logs: %s", result.logs)), nil
			}
		default:
			time.Sleep(2 * time.Second)
			if progressToken != "" {
				if progress >= 90 && progress < 100 {
					progress = progress + 1
				} else {
					progress = progress + 5
				}
				if err := server.SendNotificationToClient(
					"notifications/progress",
					map[string]interface{}{
						"progress":      progress,
						"total":         int(steps),
						"progressToken": progressToken,
					},
				); err != nil {
					server.SendNotificationToClient("notifications/error", map[string]interface{}{
						"message": fmt.Sprintf("Failed to send progress: %v", err),
					})
				}
			}
		}
	}
}

func runInDocker(ctx context.Context, cmd []string, dockerImage string, code string, language languages.Language, outputPath string) (string, []string, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Pull the Docker image
	reader, err := cli.ImagePull(ctx, dockerImage, image.PullOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("failed to pull Docker image %s: %w", dockerImage, err)
	}
	defer reader.Close()

	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return "", nil, fmt.Errorf("failed to copy Docker image pull output: %w", err)
	}

	// Create a temporary directory for the code file
	tmpDir, err := os.MkdirTemp("", "docker-sandbox-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}

	// Only remove the tmpDir when done
	defer os.RemoveAll(tmpDir)

	// Create artifacts directory
	artifactsDir := filepath.Join(tmpDir, "artifacts")
	if err := os.Mkdir(artifactsDir, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create artifacts directory: %w", err)
	}

	// Write the code to a file in the temporary directory
	tmpFile := filepath.Join(tmpDir, "main."+languages.SupportedLanguages[language].FileExtension)
	err = os.WriteFile(tmpFile, []byte(code), 0644)
	if err != nil {
		return "", nil, fmt.Errorf("failed to write code to temporary file: %w", err)
	}

	// Parse imports to detect required packages
	var packages []string
	if language == languages.Python {
		packages = languages.ParsePythonImports(code)
		fmt.Printf("Detected Python packages: %v\n", packages)
	} else if language == languages.NodeJS {
		packages = languages.ParseNodeImports(code)
	} else if language == languages.Go {
		packages = languages.ParseGoImports(code)
	}

	// Create a requirements.txt file if Python packages are detected
	if language == languages.Python && len(packages) > 0 {
		requirementsPath := filepath.Join(tmpDir, "requirements.txt")
		requirementsContent := strings.Join(packages, "\n")
		fmt.Printf("Writing requirements file to %s with content:\n%s\n", requirementsPath, requirementsContent)
		if err := os.WriteFile(requirementsPath, []byte(requirementsContent), 0644); err != nil {
			return "", nil, fmt.Errorf("failed to write requirements file: %w", err)
		}
	} else if language == languages.Python {
		fmt.Printf("No Python packages detected in imports\n")
	}

	// Modify the command to install dependencies first if needed
	var finalCmd []string
	if language == languages.Python && len(packages) > 0 {
		// Install dependencies first using uv (faster than pip), then run the code
		installCmd := "uv pip install --system " + strings.Join(packages, " ") + " && " + strings.Join(cmd, " ")
		fmt.Printf("Using install command: %s\n", installCmd)
		finalCmd = []string{
			"/bin/sh",
			"-c",
			installCmd,
		}
	} else {
		finalCmd = cmd
	}

	// Create container config
	env := []string{"ARTIFACTS_DIR=/artifacts"}

	// Mount the temporary directory to /app and artifacts directory to /artifacts
	binds := []string{
		fmt.Sprintf("%s:/app", tmpDir),
		fmt.Sprintf("%s:/artifacts", artifactsDir),
	}

	// We'll use the artifactsDir for both resource registration and direct access
	// This simplifies our approach by having a single source of truth
	if outputPath != "" {
		fmt.Printf("Using artifacts directory for both resource registration and direct file access to %s\n", outputPath)
	}

	// Add direct binding for user artifacts directory if specified
	userArtifactsDir := os.Getenv("ARTIFACTS_DIR")
	if userArtifactsDir != "" {
		// Create user artifacts directory if it doesn't exist
		if _, err := os.Stat(userArtifactsDir); os.IsNotExist(err) {
			if err := os.MkdirAll(userArtifactsDir, 0755); err != nil {
				fmt.Printf("Warning: Failed to create user artifacts directory %s: %v\n", userArtifactsDir, err)
			} else {
				fmt.Printf("Created user artifacts directory: %s\n", userArtifactsDir)
			}
		}

		// Add direct binding from container's /user-artifacts to the user-specified directory
		binds = append(binds, fmt.Sprintf("%s:/user-artifacts", userArtifactsDir))
		// Add environment variable so the container code knows about the user artifacts directory
		env = append(env, "USER_ARTIFACTS_DIR=/user-artifacts")
		fmt.Printf("Added direct binding for user artifacts: %s -> /user-artifacts\n", userArtifactsDir)
	}

	config := &container.Config{
		Image: dockerImage,
		Cmd:   finalCmd,
		Tty:   false,
		// Set environment variables
		Env: env,
	}

	hostConfig := &container.HostConfig{
		Binds: binds,
	}

	// Update container config to work in the mounted directory
	config.WorkingDir = "/app"

	sandboxContainer, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, "")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create container: %w", err)
	}

	if err := cli.ContainerStart(ctx, sandboxContainer.ID, container.StartOptions{}); err != nil {
		return "", nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for container to finish
	statusCh, errCh := cli.ContainerWait(ctx, sandboxContainer.ID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			panic(err)
		}
	case <-statusCh:
	}

	out, err := cli.ContainerLogs(ctx, sandboxContainer.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", nil, fmt.Errorf("failed to get container logs: %w", err)
	}
	defer out.Close()

	var b strings.Builder
	_, err = stdcopy.StdCopy(&b, &b, out)
	if err != nil {
		return "", nil, fmt.Errorf("failed to copy container output: %w", err)
	}

	// Use the centralized artifact collection function
	// Pass outputPath as the specified output directory (if provided)
	// or empty string if no special output path requested
	artifactURIs, err := resources.CollectArtifactsFromDir(sandboxContainer.ID, artifactsDir, outputPath)
	if err != nil {
		return b.String(), nil, fmt.Errorf("failed to collect artifacts: %w", err)
	}

	// DIRECT ARTIFACT COPY FOR DEBUGGING
	// This is a fallback direct copy mechanism to ensure artifacts are copied correctly
	if outputPath != "" {
		files, err := os.ReadDir(artifactsDir)
		if err == nil && len(files) > 0 {
			fmt.Printf("DIRECT COPY: Attempting direct copy of artifacts to %s\n", outputPath)

			// Make sure the output directory exists
			if err := os.MkdirAll(outputPath, 0755); err != nil {
				fmt.Printf("DIRECT COPY ERROR: Failed to create output directory: %v\n", err)
			} else {
				// Copy each file directly
				for _, file := range files {
					if file.IsDir() {
						continue
					}

					srcPath := filepath.Join(artifactsDir, file.Name())
					dstPath := filepath.Join(outputPath, file.Name())

					// Read source
					data, err := os.ReadFile(srcPath)
					if err != nil {
						fmt.Printf("DIRECT COPY ERROR: Failed to read artifact %s: %v\n", file.Name(), err)
						continue
					}

					// Write to destination
					if err := os.WriteFile(dstPath, data, 0644); err != nil {
						fmt.Printf("DIRECT COPY ERROR: Failed to write to %s: %v\n", dstPath, err)
					} else {
						fmt.Printf("DIRECT COPY SUCCESS: Copied %s to %s\n", file.Name(), dstPath)
					}
				}
			}
		}
	}

	return b.String(), artifactURIs, nil
}
