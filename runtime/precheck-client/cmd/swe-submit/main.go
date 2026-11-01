package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	client "example.com/swe-precheck-client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	planPath := flag.String("plan", "", "local submission plan JSON")
	schemaPath := flag.String("schema", "", "observed platform form schema JSON (dry-run)")
	execute := flag.Bool("execute", false, "upload attachments and submit; default is local dry-run")
	flag.Parse()
	if *execute {
		return fmt.Errorf("正式提交已迁移到 swe-flow submit -state ... -execute；统一状态和真实证据审核不可跳过")
	}
	if *planPath == "" {
		return fmt.Errorf("-plan is required")
	}
	var plan client.SubmissionPlan
	if err := readJSON(*planPath, &plan); err != nil {
		return err
	}
	// Resolve paths relative to the plan so the plan is portable.
	for key, path := range plan.Attachments {
		if !filepath.IsAbs(path) {
			plan.Attachments[key] = filepath.Join(filepath.Dir(*planPath), path)
		}
	}
	var schema client.FormSchema
	var c *client.Client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if *execute {
		var err error
		c, err = client.New("", nil)
		if err != nil {
			return err
		}
		if session, csrf := os.Getenv("SWE_SESSION"), os.Getenv("SWE_CSRF"); session != "" && csrf != "" {
			c.SetSessionCookies(session, csrf)
		} else {
			username, password := os.Getenv("SWE_USERNAME"), os.Getenv("SWE_PASSWORD")
			if username == "" || password == "" {
				return fmt.Errorf("provide credentials through environment variables")
			}
			if _, err := c.Login(ctx, username, password); err != nil {
				return fmt.Errorf("platform login failed; no uploads attempted")
			}
		}
		schema, err = c.SubmissionFormSchema(ctx)
		if err != nil {
			return fmt.Errorf("could not read current platform schema; no uploads attempted")
		}
	} else {
		if *schemaPath == "" {
			return fmt.Errorf("-schema is required for local dry-run")
		}
		if err := readJSON(*schemaPath, &schema); err != nil {
			return err
		}
	}
	if err := client.ValidateSubmissionPlan(plan, schema); err != nil {
		return err
	}
	keys := make([]string, 0, len(plan.Attachments))
	for key := range plan.Attachments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		file, err := os.Open(plan.Attachments[key])
		if err != nil {
			return fmt.Errorf("cannot read attachment %q", key)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		_ = file.Close()
		if copyErr != nil {
			return fmt.Errorf("cannot hash attachment %q", key)
		}
		fmt.Printf("attachment=%s file=%s bytes=%d sha256=%x\n", key, filepath.Base(plan.Attachments[key]), size, hash.Sum(nil))
	}
	if !*execute {
		fmt.Println("dry_run=PASS network_writes=0 remarks=excluded")
		// Never print complete telemetry/session tokens, even in a local dry-run.
		preview := make(map[string]any, len(plan.Payload))
		for key, value := range plan.Payload {
			if key == "trae_session_id" || key == "trae_session_id_2" {
				preview[key] = "[redacted]"
			} else {
				preview[key] = value
			}
		}
		if plan.ExistingSubmissionID > 0 {
			fmt.Printf("existing_submission_id=%d create_blocked=true\n", plan.ExistingSubmissionID)
		}
		return json.NewEncoder(os.Stdout).Encode(preview)
	}
	result, err := c.SubmitPlan(ctx, plan, schema)
	if err != nil {
		return fmt.Errorf("submission failed or outcome is uncertain; inspect platform before retrying")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read local JSON input")
	}
	return json.Unmarshal(data, target)
}
