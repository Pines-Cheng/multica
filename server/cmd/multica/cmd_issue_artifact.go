package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/issueartifact"
	"github.com/spf13/cobra"
)

func init() {
	issueCmd.AddCommand(newIssueArtifactCmd())
}

func newIssueArtifactCmd() *cobra.Command {
	return newIssueArtifactCmdWithLark(runArtifactLarkCLI)
}

func newIssueArtifactCmdWithLark(run issueartifact.LarkCommand) *cobra.Command {
	root := &cobra.Command{Use: "artifact", Short: "Manage issue artifacts with display titles and links"}
	for _, op := range []string{"list", "set", "delete"} {
		cmd := &cobra.Command{Use: op + " <issue-id>", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return runIssueArtifact(cmd, args, run) }}
		cmd.Flags().String("output", "table", "Output format: table or json")
		if op != "list" {
			cmd.Flags().String("key", "", "Stable artifact key (required)")
			_ = cmd.MarkFlagRequired("key")
		}
		switch op {
		case "list":
			cmd.Short = "List published artifacts"
		case "set":
			cmd.Short = "Create or replace an artifact by key"
			cmd.Example = `  multica issue artifact set DEV-43 --key design --title "技术方案" --url "https://example.feishu.cn/docx/..." --kind design --generated`
			cmd.Flags().String("title", "", "Display title (required)")
			cmd.Flags().String("url", "", "HTTP(S) link (required)")
			cmd.Flags().String("kind", "", "Optional classification; any text is accepted")
			cmd.Flags().Bool("generated", false, "Share a task-generated Feishu/Lark document with task readers before publishing (uses lark-cli as user)")
			cmd.Flags().Bool("reference", false, "Register pre-existing reference material without changing its permissions")
			cmd.MarkFlagsMutuallyExclusive("generated", "reference")
			_ = cmd.MarkFlagRequired("title")
			_ = cmd.MarkFlagRequired("url")
		case "delete":
			cmd.Short = "Remove an artifact by key"
		}
		root.AddCommand(cmd)
	}
	return root
}

func runIssueArtifact(cmd *cobra.Command, args []string, run issueartifact.LarkCommand) error {
	op := cmd.Name()
	key, _ := cmd.Flags().GetString("key")
	if op != "list" {
		if err := issueartifact.ValidateKey(key); err != nil {
			return err
		}
	}
	var artifact issueartifact.Artifact
	if op == "set" {
		artifact.Title, _ = cmd.Flags().GetString("title")
		artifact.URL, _ = cmd.Flags().GetString("url")
		artifact.Kind, _ = cmd.Flags().GetString("kind")
		if err := artifact.Validate(); err != nil {
			return err
		}
		generated, _ := cmd.Flags().GetBool("generated")
		reference, _ := cmd.Flags().GetBool("reference")
		if issueartifact.IsLarkDocumentURL(artifact.URL) && !generated && !reference {
			return fmt.Errorf("Feishu/Lark documents require --generated for task output or --reference for pre-existing material")
		}
		if generated && !issueartifact.IsLarkDocumentURL(artifact.URL) {
			return fmt.Errorf("--generated requires a Feishu/Lark document URL")
		}
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	budget := cli.APITimeout()
	if generated, _ := cmd.Flags().GetBool("generated"); generated {
		budget = cli.AtLeastAPITimeout(5 * time.Minute)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), budget)
	defer cancel()
	issue, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	path := "/api/issues/" + url.PathEscape(issue.ID) + "/artifacts"
	var response struct {
		Artifacts []issueartifact.Record `json:"artifacts"`
	}
	switch op {
	case "list":
		err = client.GetJSON(ctx, path, &response)
	case "set":
		generated, _ := cmd.Flags().GetBool("generated")
		if generated {
			var audience issueartifact.Audience
			if err := client.GetJSON(ctx, "/api/issues/"+url.PathEscape(issue.ID)+"/artifact-audience", &audience); err != nil {
				return fmt.Errorf("resolve document readers: %w", err)
			}
			if err := issueartifact.ShareGeneratedDocument(ctx, run, artifact.URL, audience); err != nil {
				return fmt.Errorf("document access is not ready; artifact was not published: %w", err)
			}
		}
		err = client.PutJSON(ctx, path+"/"+url.PathEscape(key), artifact, &response)
	case "delete":
		err = client.DeleteJSON(ctx, path+"/"+url.PathEscape(key))
	}
	if err != nil {
		return fmt.Errorf("%s artifact: %w", op, err)
	}
	output, _ := cmd.Flags().GetString("output")
	if op == "delete" {
		if output == "json" {
			return cli.PrintJSON(os.Stdout, map[string]any{"deleted": true, "key": key})
		}
		fmt.Fprintf(os.Stdout, "Artifact %s deleted.\n", key)
		return nil
	}
	if response.Artifacts == nil {
		response.Artifacts = []issueartifact.Record{}
	}
	if output == "json" {
		return cli.PrintJSON(os.Stdout, response.Artifacts)
	}
	rows := make([][]string, 0, len(response.Artifacts))
	for _, a := range response.Artifacts {
		rows = append(rows, []string{a.Key, a.Title, a.Kind, a.URL})
	}
	cli.PrintTable(os.Stdout, []string{"KEY", "TITLE", "KIND", "URL"}, rows)
	return nil
}

func runArtifactLarkCLI(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "lark-cli", args...)
	// Failure envelopes may be written to stderr by lark-cli. Keep stdout
	// clean on success, and return the structured error on failure.
	data, err := command.Output()
	if exit, ok := err.(*exec.ExitError); ok && len(data) == 0 {
		data = exit.Stderr
	}
	return data, err
}
