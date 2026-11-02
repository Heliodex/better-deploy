package main

import (
	"encoding/json/v2"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type blobRecord struct {
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	CreatedAt   time.Time `json:"createdAt"`
}

type blobsResponse struct {
	Blobs []blobRecord `json:"blobs"`
}

type blobGCResult struct {
	Deleted   []string `json:"deleted"`
	DryRun    bool     `json:"dryRun"`
	OlderThan string   `json:"olderThan"`
}

// runBlobsList prints an app's stored blobs.
func runBlobsList(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("blobs list", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/blobs", nil, "")
	if err != nil {
		return err
	}
	if err := checkAPI(status, body); err != nil {
		return err
	}
	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}
	var result blobsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if len(result.Blobs) == 0 {
		fmt.Println("no blobs")
		return nil
	}
	fmt.Printf("%-64s %-12s %-28s %s\n", "DIGEST", "SIZE", "CONTENT TYPE", "CREATED")
	for _, blob := range result.Blobs {
		fmt.Printf("%-64s %-12d %-28s %s\n", blob.Digest, blob.Size, blob.ContentType, jobTime(blob.CreatedAt))
	}
	return nil
}

// runBlobsDelete removes one blob by digest.
func runBlobsDelete(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("blobs delete", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: wt blobs <app> delete <digest>")
	}
	digest := flags.Arg(0)
	status, _, body, err := apiDo(cfg, http.MethodDelete, "/apps/"+name+"/blobs/"+digest, nil, "")
	if err != nil {
		return err
	}
	if err := checkAPI(status, body); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", digest)
	return nil
}

// runBlobsGC removes old blobs or previews the removal.
func runBlobsGC(cfg clientOptions, name string, args []string) error {
	flags := flag.NewFlagSet("blobs gc", flag.ContinueOnError)
	olderThan := flags.String("older-than", "", "minimum age, for example 720h")
	dryRun := flags.Bool("dry-run", false, "report what would be removed")
	keep := []string{}
	flags.Func("keep", "digest to protect (repeatable or comma-separated)", func(value string) error {
		for digest := range strings.SplitSeq(value, ",") {
			if digest = strings.TrimSpace(digest); digest != "" {
				keep = append(keep, digest)
			}
		}
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *olderThan == "" {
		return fmt.Errorf("--older-than is required")
	}
	if duration, err := time.ParseDuration(*olderThan); err != nil || duration <= 0 {
		return fmt.Errorf("--older-than must be a positive duration")
	}
	payload := map[string]any{"olderThan": *olderThan, "dryRun": *dryRun}
	if len(keep) > 0 {
		payload["keep"] = keep
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/blobs/gc", strings.NewReader(string(data)), "application/json")
	if err != nil {
		return err
	}
	if err := checkAPI(status, body); err != nil {
		return err
	}
	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}
	var result blobGCResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	verb := "deleted"
	if result.DryRun {
		verb = "would delete"
	}
	fmt.Printf("%s %d blob(s) older than %s\n", verb, len(result.Deleted), result.OlderThan)
	for _, digest := range result.Deleted {
		fmt.Println(digest)
	}
	return nil
}

// runBlobs dispatches wt blobs <app> <list|delete|gc>.
func runBlobs(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: wt blobs <app> <list|delete|gc>")
	}
	name, action := rest[0], rest[1]
	if !validName(name) {
		return fmt.Errorf("invalid app name %q", name)
	}
	switch action {
	case "list":
		return runBlobsList(cfg, name, rest[2:])
	case "delete":
		return runBlobsDelete(cfg, name, rest[2:])
	case "gc":
		return runBlobsGC(cfg, name, rest[2:])
	default:
		return fmt.Errorf("unknown blobs action %q", action)
	}
}
