package main

import (
	"context"
	"encoding/json"

	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type config struct {
	source, bucket, prefix string
	delete, dry            bool
	timeout                time.Duration
}

func loadConfig() (config, error) {
	c := config{source: env("BACKUP_SOURCE", "/source"), bucket: os.Getenv("S3_BUCKET"), prefix: strings.Trim(os.Getenv("S3_PREFIX"), "/")}
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if os.Getenv(key) == "" {
			return c, fmt.Errorf("required environment variable: %s", key)
		}
	}
	if c.prefix == "" {
		return c, fmt.Errorf("S3_PREFIX must be a dedicated non-empty prefix")
	}
	for _, part := range strings.Split(c.prefix, "/") {
		if part == "." || part == ".." || part == "" {
			return c, fmt.Errorf("invalid S3_PREFIX")
		}
	}
	u, err := url.Parse(os.Getenv("S3_ENDPOINT"))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return c, fmt.Errorf("S3_ENDPOINT must be an HTTPS service URL without bucket/path or credentials")
	}
	if strings.ContainsAny(c.bucket, "/\\: \t\r\n") || strings.HasPrefix(c.bucket, "-") {
		return c, fmt.Errorf("invalid S3_BUCKET")
	}
	if !filepath.IsAbs(c.source) {
		return c, fmt.Errorf("BACKUP_SOURCE must be absolute")
	}
	c.delete, err = strconv.ParseBool(env("BACKUP_DELETE", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid BACKUP_DELETE")
	}
	c.dry, err = strconv.ParseBool(env("BACKUP_DRY_RUN", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid BACKUP_DRY_RUN")
	}
	c.timeout, err = time.ParseDuration(env("BACKUP_TIMEOUT", "2h"))
	if err != nil || c.timeout <= 0 {
		return c, fmt.Errorf("invalid BACKUP_TIMEOUT")
	}
	return c, nil
}

func sourceFiles(source string) ([]string, error) {
	marker, err := os.Lstat(filepath.Join(source, ".s3-backup-source"))
	if err != nil || !marker.Mode().IsRegular() {
		return nil, fmt.Errorf("source marker missing: check source mount")
	}
	var files []string
	err = filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == source || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink unsupported: %s (no deletion performed)", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file unsupported: %s", p)
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		if rel != ".s3-backup-source" {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

type awsRunner func(context.Context, []string, io.Writer) error

func executeAWS(ctx context.Context, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, env("AWS_CLI_BIN", "aws"), args...)
	cmd.Stdout = output
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("AWS CLI interrupted: %w", ctx.Err())
		}
		return fmt.Errorf("AWS CLI failed: %w", err)
	}
	return nil
}

func run(ctx context.Context) error { return runWithAWS(ctx, executeAWS) }

func runWithAWS(ctx context.Context, aws awsRunner) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	files, err := sourceFiles(c.source)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("source has no files; refusing backup")
	}
	base := []string{"--no-cli-pager", "--endpoint-url", os.Getenv("S3_ENDPOINT"), "--region", env("AWS_REGION", "us-east-1")}
	if c.delete {
		var output strings.Builder
		args := append(append([]string{}, base...), "s3api", "get-bucket-versioning", "--bucket", c.bucket, "--output", "json")
		if err := aws(ctx, args, &output); err != nil {
			return err
		}
		var version struct{ Status string }
		if err := json.Unmarshal([]byte(output.String()), &version); err != nil {
			return fmt.Errorf("invalid versioning response: %w", err)
		}
		if version.Status != "Enabled" {
			return fmt.Errorf("BACKUP_DELETE requires Enabled bucket versioning")
		}
	}
	args := append(append([]string{}, base...), "s3", "sync", filepath.Clean(c.source)+string(os.PathSeparator), "s3://"+c.bucket+"/"+c.prefix+"/", "--no-follow-symlinks", "--exclude", ".s3-backup-source", "--no-progress")
	if c.delete {
		args = append(args, "--delete")
	}
	if c.dry {
		args = append(args, "--dryrun")
	}
	log.Printf("backup started files=%d prefix=%q delete=%t dry_run=%t", len(files), c.prefix, c.delete, c.dry)
	if err := aws(ctx, args, os.Stdout); err != nil {
		return err
	}
	log.Print("backup completed")
	return nil
}

func main() {
	zone, err := time.LoadLocation(env("BACKUP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		log.Fatal(err)
	}
	log.SetFlags(0)
	log.SetOutput(zoneWriter{zone})
	if _, err = loadConfig(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := exec.LookPath(env("AWS_CLI_BIN", "aws")); err != nil {
		log.Fatalf("AWS CLI not found: %v", err)
	}
	value := os.Getenv("BACKUP_INTERVAL")
	if value == "" {
		if err = run(ctx); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	duration, durationErr := time.ParseDuration(value)
	var schedule cron.Schedule
	if durationErr != nil {
		schedule, err = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(value)
		if err != nil {
			log.Fatal(err)
		}
	} else if duration <= 0 {
		log.Fatal("BACKUP_INTERVAL must be positive")
	}
	for ctx.Err() == nil {
		if schedule != nil {
			next := schedule.Next(time.Now().In(zone))
			if next.IsZero() {
				log.Fatal("cron has no future execution")
			}
			log.Printf("next backup: %s", next.Format(time.RFC3339))
			if !wait(ctx, time.Until(next)) {
				return
			}
		}
		if err := run(ctx); err != nil {
			log.Printf("backup failed: %v", err)
		}
		if schedule == nil && !wait(ctx, duration) {
			return
		}
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

type zoneWriter struct{ zone *time.Location }

func (w zoneWriter) Write(p []byte) (int, error) {
	_, err := fmt.Fprintf(os.Stderr, "%s %s", time.Now().In(w.zone).Format(time.RFC3339), p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
