// drift-watch plans reference image bumps and a pull request for API drift.
// Lab credentials stay in the environment and in gitignored lab files.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: drift-watch images|report|pr-body|should-open")
	}
	switch args[0] {
	case "images":
		return cmdImages(args[1:])
	case "report":
		return cmdReport(args[1:])
	case "pr-body":
		return cmdPRBody(args[1:])
	case "should-open":
		return cmdShouldOpen(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdImages(args []string) error {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	composePath := fs.String("compose", "scripts/lab/compose.yaml", "lab compose file")
	write := fs.Bool("write", false, "rewrite service image pins")
	out := fs.String("out", "", "write the image plan JSON here")
	github := fs.String("github-api", "https://api.github.com", "GitHub API origin")
	dockerAuth := fs.String("docker-auth", "https://auth.docker.io", "Docker Hub auth origin")
	dockerRegistry := fs.String("docker-registry", "https://registry-1.docker.io", "Docker Hub registry origin")
	ghcr := fs.String("ghcr", "https://ghcr.io", "GHCR origin")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: drift-watch images [-compose scripts/lab/compose.yaml] [-write] [-out images.json]")
	}
	compose, err := os.ReadFile(*composePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := defaultResolver()
	r.githubAPI = *github
	r.dockerAuth = *dockerAuth
	r.dockerRegistry = *dockerRegistry
	r.ghcr = *ghcr
	updated, changes, err := r.plan(ctx, string(compose))
	if err != nil {
		return err
	}
	if *write && updated != string(compose) {
		info, err := os.Stat(*composePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*composePath, []byte(updated), info.Mode().Perm()); err != nil {
			return err
		}
	}
	if *out == "" {
		return nil
	}
	body, err := json.MarshalIndent(changes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(body, '\n'), 0o644)
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	api := fs.String("api", "", "api-drift markdown")
	images := fs.String("images", "", "image plan JSON")
	blocking := fs.String("blocking", "", "directory of conformdiff JSON reports")
	recorded := fs.Bool("recorded", false, "a corpus re-record was attempted")
	recordError := fs.String("record-error", "", "file containing a secret-free record failure")
	out := fs.String("out", "", "report JSON path")
	if err := fs.Parse(args); err != nil || *api == "" || *images == "" || *out == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: drift-watch report -api api.md -images images.json -out report.json [-blocking dir] [-recorded] [-record-error file]")
	}
	markdown, err := os.ReadFile(*api)
	if err != nil {
		return err
	}
	rawImages, err := os.ReadFile(*images)
	if err != nil {
		return err
	}
	var changes []imageChange
	if err := json.Unmarshal(rawImages, &changes); err != nil {
		return err
	}
	doc := reportDoc{
		APIDriftMarkdown: strings.TrimSpace(string(markdown)),
		Images:           changes,
		Recorded:         *recorded,
	}
	if *recordError != "" {
		note, err := os.ReadFile(*recordError)
		if err != nil {
			return err
		}
		doc.RecordError = strings.TrimSpace(string(note))
		doc.Recorded = true
	}
	if *blocking != "" {
		rows, err := loadBlockingDir(*blocking)
		if err != nil {
			return err
		}
		doc.Blocking = rows
		doc.Recorded = true
	}
	doc.RecordNote = recordNote(doc.Recorded)
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(body, '\n'), 0o644)
}

func cmdPRBody(args []string) error {
	fs := flag.NewFlagSet("pr-body", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	report := fs.String("report", "", "report JSON")
	out := fs.String("out", "", "markdown output, or stdout when empty")
	if err := fs.Parse(args); err != nil || *report == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: drift-watch pr-body -report report.json [-out body.md]")
	}
	doc, err := readReport(*report)
	if err != nil {
		return err
	}
	body := renderPRBody(doc, time.Now())
	if *out == "" {
		_, err := io.WriteString(os.Stdout, body)
		return err
	}
	return os.WriteFile(*out, []byte(body), 0o644)
}

func cmdShouldOpen(args []string) error {
	fs := flag.NewFlagSet("should-open", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	report := fs.String("report", "", "report JSON")
	if err := fs.Parse(args); err != nil || *report == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: drift-watch should-open -report report.json")
	}
	doc, err := readReport(*report)
	if err != nil {
		return err
	}
	if shouldOpen(doc) {
		fmt.Println("yes")
	} else {
		fmt.Println("no")
	}
	return nil
}

func readReport(path string) (reportDoc, error) {
	var doc reportDoc
	body, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return doc, err
	}
	return doc, nil
}
