package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/issueartifact"
	"github.com/spf13/cobra"
)

func init() {
	issueCmd.AddCommand(newIssueArtifactCmd())
}

func newIssueArtifactCmd() *cobra.Command {
	root := &cobra.Command{Use: "artifact", Short: "Manage issue artifacts with display titles and links"}
	for _, op := range []string{"list", "set", "delete"} {
		cmd := &cobra.Command{Use: op + " <issue-id>", Args: exactArgs(1), RunE: runIssueArtifact}
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
			cmd.Example = `  multica issue artifact set DEV-43 --key design --title "技术方案" --url "https://example.feishu.cn/docx/..." --kind design`
			cmd.Flags().String("title", "", "Display title (required)")
			cmd.Flags().String("url", "", "HTTP(S) link (required)")
			cmd.Flags().String("kind", "", "Optional classification; any text is accepted")
			_ = cmd.MarkFlagRequired("title")
			_ = cmd.MarkFlagRequired("url")
		case "delete":
			cmd.Short = "Remove an artifact by key"
		}
		root.AddCommand(cmd)
	}
	return root
}

func runIssueArtifact(cmd *cobra.Command, args []string) error {
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
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
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
