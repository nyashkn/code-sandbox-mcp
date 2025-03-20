package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Automata-Labs-team/code-sandbox-mcp/installer"
	deps "github.com/Automata-Labs-team/code-sandbox-mcp/languages"
	"github.com/Automata-Labs-team/code-sandbox-mcp/resources"
	"github.com/Automata-Labs-team/code-sandbox-mcp/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// GenerateEnumTag generates the jsonschema enum tag for all supported languages
func GenerateEnumTag() string {
	var tags []string
	for _, lang := range deps.AllLanguages {
		tags = append(tags, fmt.Sprintf("enum=%s", lang))
	}
	return strings.Join(tags, ",")
}

func init() {
	// Check for --install flag
	installFlag := flag.Bool("install", false, "Add this binary to Claude Desktop config")
	noUpdateFlag := flag.Bool("no-update", false, "Disable auto-update check")
	flag.Parse()

	if *installFlag {
		if err := installer.InstallConfig(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Check for updates unless disabled
	if !*noUpdateFlag {
		if hasUpdate, downloadURL, err := installer.CheckForUpdate(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to check for updates: %v\n", err)
			os.Exit(1)
		} else if hasUpdate {
			fmt.Println("Updating to new version...")
			if err := installer.PerformUpdate(downloadURL); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to update: %v\n", err)
			}
			fmt.Println("Update complete. Restarting...")
		}
	}
}

func main() {
	port := flag.String("port", "9520", "Port to listen on")
	transport := flag.String("transport", "stdio", "Transport to use (stdio, sse)")
	flag.Parse()
	s := server.NewMCPServer("code-sandbox-mcp", "v1.0.0", server.WithLogging(), server.WithResourceCapabilities(true, true), server.WithPromptCapabilities(false))
	s.AddNotificationHandler("notifications/error", handleNotification)

	// Register a tool to run code in a docker container
	runCodeTool := mcp.NewTool("run_code",
		mcp.WithDescription(
			"Run code in a sandboxed docker container with automatic dependency detection and installation. \n"+
				"The tool will analyze your code and install required packages automatically. \n"+
				"You can also specify dependencies using a special comment: \n"+
				"  # requirements: package1, package2==1.0.0, package3>=2.0.0 \n"+
				"The supported languages are: "+GenerateEnumTag()+". \n"+
				"Returns the execution logs of the container and any generated artifacts.\n\n"+
				"To save output files, write them to the /artifacts directory:\n"+
				"Example: `plt.savefig('/artifacts/plot.png')`\n\n"+
				"You can specify an outputPath parameter to save artifacts to a specific directory.",
		),
		mcp.WithString("code",
			mcp.Required(),
			mcp.Description("The code to run"),
		),
		mcp.WithString("language",
			mcp.Required(),
			mcp.Description("The programming language to use"),
			mcp.Enum(deps.AllLanguages.ToArray()...),
		),
		mcp.WithString("outputPath",
			mcp.Description("Optional full path to a directory where artifacts will be saved"),
		),
	)

	runProjectTool := mcp.NewTool("run_project",
		mcp.WithDescription(
			"Run a project in a sandboxed docker container. \n"+
				"The tool will install required packages automatically. \n"+
				"For run_code, you can specify dependencies using a special comment: \n"+
				"  # requirements: package1, package2==1.0.0, package3>=2.0.0 \n"+
				"The supported languages are: "+GenerateEnumTag()+". \n"+
				"Returns the resource URI of the container logs and any generated artifacts.\n\n"+
				"To save output files, write them to the /artifacts directory:\n"+
				"Example: `plt.savefig('/artifacts/plot.png')`",
		),
		mcp.WithString("projectDir",
			mcp.Required(),
			mcp.Description("Location of the project to run"),
		),
		mcp.WithString("language",
			mcp.Required(),
			mcp.Description("The programming language to use"),
			mcp.Enum(deps.AllLanguages.ToArray()...),
		),
		mcp.WithString("entrypointCmd",
			mcp.Required(),
			mcp.Description("Entrypoint command to run at the root of the project directory."),
			mcp.Description("Examples: `npm run dev`, `python main.py`, `go run main.go`"),
		),
	)

	// Register dynamic resource for container logs
	// Dynamic resource example - Container Logs by ID
	containerLogsTemplate := mcp.NewResourceTemplate(
		"containers://{id}/logs",
		"Container Logs",
		mcp.WithTemplateDescription("Returns all container logs from the specified container. Logs are returned as a single text resource."),
		mcp.WithTemplateMIMEType("text/plain"),
		mcp.WithTemplateAnnotations([]mcp.Role{mcp.RoleAssistant, mcp.RoleUser}, 0.5),
	)

	// Register dynamic resource for container artifacts
	containerArtifactsTemplate := mcp.NewResourceTemplate(
		"artifacts://{containerid}/{filename}",
		"Container Artifacts",
		mcp.WithTemplateDescription("Returns file artifacts generated during code execution. Supports images, PDFs, and other file types."),
		mcp.WithTemplateAnnotations([]mcp.Role{mcp.RoleAssistant, mcp.RoleUser}, 0.5),
	)

	s.AddResourceTemplate(containerLogsTemplate, resources.GetContainerLogs)
	s.AddResourceTemplate(containerArtifactsTemplate, resources.GetContainerArtifact)
	s.AddTool(runCodeTool, tools.RunCodeSandbox)
	s.AddTool(runProjectTool, tools.RunProjectSandbox)

	switch *transport {
	case "stdio":
		if err := server.ServeStdio(s); err != nil {
			s.SendNotificationToClient("notifications/error", map[string]interface{}{
				"message": fmt.Sprintf("Failed to start stdio server: %v", err),
			})
		}
	case "sse":
		sseServer := server.NewSSEServer(s, fmt.Sprintf("http://localhost:%s", *port))
		if err := sseServer.Start(fmt.Sprintf(":%s", *port)); err != nil {
			s.SendNotificationToClient("notifications/error", map[string]interface{}{
				"message": fmt.Sprintf("Failed to start SSE server: %v", err),
			})
		}
	default:
		s.SendNotificationToClient("notifications/error", map[string]interface{}{
			"message": fmt.Sprintf("Invalid transport: %s", *transport),
		})
	}
}

func handleNotification(
	ctx context.Context,
	notification mcp.JSONRPCNotification,
) {
	log.Printf("Received notification from client: %s", notification.Method)
}
