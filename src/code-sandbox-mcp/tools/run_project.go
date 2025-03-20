package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	deps "github.com/Automata-Labs-team/code-sandbox-mcp/languages"
	resources "github.com/Automata-Labs-team/code-sandbox-mcp/resources"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/moby/moby/client"
)

// extractRequirementsFromPythonFiles scans all Python files in a directory
// and extracts requirements from comments formatted as "# requirements: package1, package2"
func extractRequirementsFromPythonFiles(projectDir string) ([]string, error) {
	requirementsRe := regexp.MustCompile(`(?m)^#\s*requirements:\s*(.+)$`)
	var allRequirements []string
	requirementsMap := make(map[string]bool)

	// Walk through all .py files in the project directory
	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories and non-Python files
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(info.Name()), ".py") {
			return nil
		}

		// Read file content
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("Warning: failed to read file %s: %v\n", path, err)
			return nil // Continue with other files
		}

		// Find requirements comments
		matches := requirementsRe.FindAllStringSubmatch(string(content), -1)
		for _, match := range matches {
			if len(match) >= 2 {
				// Split by comma and add to map to remove duplicates
				reqsList := strings.Split(match[1], ",")
				for _, req := range reqsList {
					req = strings.TrimSpace(req)
					if req != "" && !requirementsMap[req] {
						requirementsMap[req] = true
						allRequirements = append(allRequirements, req)
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to scan project files: %w", err)
	}

	return allRequirements, nil
}

func RunProjectSandbox(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var progressToken mcp.ProgressToken
	if request.Params.Meta != nil && request.Params.Meta.ProgressToken != nil {
		progressToken = request.Params.Meta.ProgressToken
	}

	language, ok := request.Params.Arguments["language"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid language")
	}
	entrypoint, ok := request.Params.Arguments["entrypointCmd"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid entrypoint")
	}
	projectDir, ok := request.Params.Arguments["projectDir"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid projectDir")
	}

	// Validate project directory
	projectDir = filepath.Clean(projectDir)
	if _, err := os.Stat(projectDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("project directory does not exist: %s", projectDir)
	}

	config := deps.SupportedLanguages[deps.Language(language)]
	containerId, artifacts, err := runProjectInDocker(ctx, progressToken, strings.Fields(entrypoint), config.Image, projectDir, deps.Language(language))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Error: %v", err)), nil
	}

	// Always include the container logs URI
	resultText := fmt.Sprintf("Resource URI: containers://%s/logs", containerId)

	// Also include artifact URIs if available
	if len(artifacts) > 0 {
		resultText += fmt.Sprintf("\n\nArtifacts: %s", strings.Join(artifacts, ", "))
	}

	return mcp.NewToolResultText(resultText), nil
}

func runProjectInDocker(ctx context.Context, progressToken mcp.ProgressToken, cmd []string, dockerImage string, projectDir string, language deps.Language) (string, []string, error) {
	server := server.ServerFromContext(ctx)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", nil, fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	if progressToken != "" {
		if err := server.SendNotificationToClient(
			"notifications/progress",
			map[string]interface{}{
				"progress":      10,
				"progressToken": progressToken,
			},
		); err != nil {
			return "", nil, fmt.Errorf("failed to send progress notification: %w", err)
		}
	}

	// Pull the Docker image
	_, err = cli.ImagePull(ctx, dockerImage, image.PullOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("failed to pull Docker image %s: %w", dockerImage, err)
	}

	// Check for dependency files and prepare install command
	var hasDepFile bool
	var depFile string

	// Look for standard dependency files first
	for _, file := range deps.SupportedLanguages[language].DependencyFiles {
		if _, err := os.Stat(filepath.Join(projectDir, file)); err == nil {
			hasDepFile = true
			depFile = file
			break
		}
	}

	// For Python projects, also check for requirements comments in .py files
	// if we didn't find a requirements.txt file
	if language == deps.Python && (!hasDepFile || depFile != "requirements.txt") {
		// Create a temporary requirements file from requirements comments
		reqsFromComments, err := extractRequirementsFromPythonFiles(projectDir)
		if err != nil {
			fmt.Printf("Warning: failed to extract requirements from Python files: %v\n", err)
		} else if len(reqsFromComments) > 0 {
			// Create or update requirements.txt file
			reqsPath := filepath.Join(projectDir, "requirements.txt")
			var existingReqs []string

			// Read existing requirements if file exists
			if _, err := os.Stat(reqsPath); err == nil {
				content, err := os.ReadFile(reqsPath)
				if err == nil {
					existingReqs = strings.Split(string(content), "\n")
				}
			}

			// Merge requirements (prioritize existing ones)
			reqsMap := make(map[string]bool)
			for _, req := range existingReqs {
				req = strings.TrimSpace(req)
				if req != "" {
					reqsMap[req] = true
				}
			}

			for _, req := range reqsFromComments {
				req = strings.TrimSpace(req)
				if req != "" && !reqsMap[req] {
					reqsMap[req] = true
				}
			}

			// Write combined requirements
			var finalReqs []string
			for req := range reqsMap {
				finalReqs = append(finalReqs, req)
			}

			err = os.WriteFile(reqsPath, []byte(strings.Join(finalReqs, "\n")), 0644)
			if err != nil {
				fmt.Printf("Warning: failed to write requirements.txt: %v\n", err)
			} else {
				hasDepFile = true
				depFile = "requirements.txt"
				fmt.Printf("Created requirements.txt from requirements comments: %v\n", finalReqs)
			}
		}
	}

	// Create two directories:
	// 1. A temporary directory for internal artifact processing
	// 2. A dedicated artifacts directory inside the project for user access
	artifactsDir, err := os.MkdirTemp("", "project-artifacts-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary artifacts directory: %w", err)
	}

	// Create artifacts directory inside project directory
	projectArtifactsDir := filepath.Join(projectDir, "artifacts")
	if err := os.MkdirAll(projectArtifactsDir, 0755); err != nil {
		os.RemoveAll(artifactsDir) // Clean up temp dir if we failed
		return "", nil, fmt.Errorf("failed to create project artifacts directory: %w", err)
	}

	fmt.Printf("Created artifacts directory in project: %s\n", projectArtifactsDir)

	// Create container config with working directory set to /app
	containerConfig := &container.Config{
		Image:      dockerImage,
		WorkingDir: "/app",
		// AttachStdout: true,
		// AttachStderr: true,
		Tty: false,
		// Set environment variable to tell the code where the artifacts directory is
		Env: []string{"ARTIFACTS_DIR=/artifacts"},
	}

	// If we have dependencies, modify the command to install them first
	if hasDepFile {
		switch language {
		case deps.Python:
			if depFile == "requirements.txt" {
				containerConfig.Cmd = []string{
					"/bin/sh", "-c", fmt.Sprintf("uv pip install --system -r %s && %s", depFile, strings.Join(cmd, " ")),
				}
			} else if depFile == "pyproject.toml" || depFile == "setup.py" {
				containerConfig.Cmd = []string{
					"/bin/sh", "-c", fmt.Sprintf("uv pip install --system . && %s", strings.Join(cmd, " ")),
				}
			}
		case deps.Go:
			// Combine the install command with the run command
			containerConfig.Cmd = append(deps.SupportedLanguages[language].InstallCommand, cmd...)
		case deps.NodeJS:
			// Bun automatically installs dependencies when running the project, so just combine "bun" with the command after index 1
			containerConfig.Cmd = append([]string{"bun"}, cmd[1:]...)
		}
	} else {
		// Handle the case where there are no dependency files
		switch language {
		case deps.Python:
			// For Python without dependencies, use shell to execute the command
			containerConfig.Cmd = []string{
				"/bin/sh", "-c", strings.Join(cmd, " "),
			}
		default:
			// For other languages, use the command as is
			containerConfig.Cmd = cmd
		}
	}

	if progressToken != "" {
		server.SendNotificationToClient(
			"notifications/progress",
			map[string]interface{}{
				"progress":      50,
				"progressToken": progressToken,
			},
		)
	}

	// Mount the project directory to /app and artifacts directory to /artifacts in the container
	// Also mount the project's artifacts directory to /app/artifacts for direct access
	binds := []string{
		fmt.Sprintf("%s:/app", projectDir),
		fmt.Sprintf("%s:/artifacts", artifactsDir),
	}

	fmt.Printf("Mounting binds: %v\n", binds)

	// Updated approach: Use artifacts directory for both MCP resources and direct access
	// This simplifies the approach by having a single source of truth
	fmt.Printf("Using artifacts directory for both resource registration and direct file access\n")

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

		// For run_project, we'll store artifacts with a project-specific subfolder
		projectName := filepath.Base(projectDir)
		userProjectArtifactsDir := filepath.Join(userArtifactsDir, projectName)
		if err := os.MkdirAll(userProjectArtifactsDir, 0755); err != nil {
			fmt.Printf("Warning: Failed to create project-specific artifacts directory %s: %v\n", userProjectArtifactsDir, err)
		} else {
			fmt.Printf("Created project-specific artifacts directory: %s\n", userProjectArtifactsDir)

			// Instead of binding, we'll directly copy artifacts to this directory later
			// This provides a simpler approach that doesn't rely on container environment variables
		}
	}

	hostConfig := &container.HostConfig{
		Binds: binds,
	}

	resp, err := cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		os.RemoveAll(artifactsDir)
		return "", nil, fmt.Errorf("failed to create container: %w", err)
	}

	if progressToken != "" {
		server.SendNotificationToClient(
			"notifications/progress",
			map[string]interface{}{
				"progress":      75,
				"progressToken": progressToken,
			},
		)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		os.RemoveAll(artifactsDir)
		return "", nil, fmt.Errorf("failed to start container: %w", err)
	}

	if progressToken != "" {
		server.SendNotificationToClient(
			"notifications/progress",
			map[string]interface{}{
				"progress":      100,
				"progressToken": progressToken,
			},
		)
	}

	// Import the actual artifacts module properly
	// Pass the project artifacts directory so files get copied there
	artifactURIs, err := resources.CollectArtifactsFromDir(resp.ID, artifactsDir, projectArtifactsDir)
	if err != nil {
		return resp.ID, nil, fmt.Errorf("failed to collect artifacts: %w", err)
	}

	return resp.ID, artifactURIs, nil
}
