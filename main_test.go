package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func configEnv(t *testing.T) {
	t.Setenv("BACKUP_SOURCE", t.TempDir())
	t.Helper()
	for k, v := range map[string]string{"S3_ENDPOINT": "https://s3.example.com", "S3_BUCKET": "backup", "S3_PREFIX": "web-01/www", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test", "BACKUP_DELETE": "false", "BACKUP_DRY_RUN": "false", "BACKUP_TIMEOUT": "2h"} {
		t.Setenv(k, v)
	}
}
func TestConfigRejectsUnsafePrefixAndBoolean(t *testing.T) {
	configEnv(t)
	for _, prefix := range []string{"", "/", "web/../other", "web//other"} {
		t.Setenv("S3_PREFIX", prefix)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("accepted prefix %q", prefix)
		}
	}
	t.Setenv("S3_PREFIX", "web-01/www")
	t.Setenv("BACKUP_DELETE", "typo")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted invalid boolean")
	}
}
func TestMissingMountMarker(t *testing.T) {
	source := t.TempDir()
	_, err := sourceFiles(source)
	if err == nil {
		t.Fatal("accepted missing mount marker")
	}
	for _, text := range []string{filepath.Join(source, ".s3-backup-source"), "未启动同步", "touch /www/wwwroot/.s3-backup-source", "不要在启动脚本中自动创建"} {
		// %q escapes Windows path separators; check the displayed quoted path separately.
		if text == filepath.Join(source, ".s3-backup-source") {
			text = strconv.Quote(text)
		}
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("missing operator guidance %q in %v", text, err)
		}
	}
}
func TestWalkCollectsFilesWithoutMarker(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".s3-backup-source", "image.jpg"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "image.jpg" {
		t.Fatalf("unexpected files %v", files)
	}
}

func TestSourceSymlinksDoNotBlockBackupOrFollowTargets(t *testing.T) {
	populatedSource(t)
	root := os.Getenv("BACKUP_SOURCE")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"storage":     outside,
		"file-link":   filepath.Join(root, "test.txt"),
		"broken-link": filepath.Join(outside, "missing"),
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
	}
	files, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "test.txt" {
		t.Fatalf("unexpected source files: %v", files)
	}
	calls := 0
	if err := runWithAWS(context.Background(), func(_ context.Context, args []string, _ io.Writer) error {
		calls++
		return validateBackupCommand(args)
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected upload sync, calls=%d", calls)
	}
}
func TestEmptySourceNeverDeletes(t *testing.T) {
	configEnv(t)
	root := t.TempDir()
	t.Setenv("BACKUP_SOURCE", root)
	t.Setenv("BACKUP_DELETE", "true")
	if err := os.WriteFile(filepath.Join(root, ".s3-backup-source"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background()); err == nil {
		t.Fatal("accepted empty source")
	}
}
func populatedSource(t *testing.T) {
	t.Helper()
	configEnv(t)
	root := t.TempDir()
	t.Setenv("BACKUP_SOURCE", root)
	for _, name := range []string{".s3-backup-source", "test.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("hello"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDryRunPreservesOfficialCLIFlags(t *testing.T) {
	populatedSource(t)
	t.Setenv("BACKUP_DRY_RUN", "true")
	var calls [][]string
	err := runWithAWS(context.Background(), func(ctx context.Context, args []string, w io.Writer) error { calls = append(calls, args); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
	command := strings.Join(calls[0], " ")
	for _, flag := range []string{"s3 sync", "--dryrun", "--no-follow-symlinks", "s3://backup/web-01/www/", "--exclude .s3-backup-source"} {
		if !strings.Contains(command, flag) {
			t.Fatalf("missing %s in %s", flag, command)
		}
	}
	if strings.Contains(command, "--delete") {
		t.Fatal("unexpected deletion")
	}
}
func TestDeletionRequiresVersioning(t *testing.T) {
	for _, status := range []string{"Suspended", "", "Enabled"} {
		t.Run(status, func(t *testing.T) {
			populatedSource(t)
			t.Setenv("BACKUP_DELETE", "true")
			calls := 0
			err := runWithAWS(context.Background(), func(ctx context.Context, args []string, w io.Writer) error {
				calls++
				if calls == 1 {
					_, err := io.WriteString(w, `{"Status":"`+status+`"}`)
					return err
				}
				if !strings.Contains(strings.Join(args, " "), "--delete") {
					t.Fatal("missing --delete")
				}
				return nil
			})
			if status == "Enabled" {
				if err != nil || calls != 2 {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
func TestCLIFailurePropagates(t *testing.T) {
	populatedSource(t)
	failure := errors.New("CLI exit 1")
	err := runWithAWS(context.Background(), func(context.Context, []string, io.Writer) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("lost error: %v", err)
	}
}
func TestInvalidVersioningResponseStopsSync(t *testing.T) {
	populatedSource(t)
	t.Setenv("BACKUP_DELETE", "true")
	calls := 0
	err := runWithAWS(context.Background(), func(ctx context.Context, args []string, w io.Writer) error {
		calls++
		_, err := io.WriteString(w, "invalid")
		return err
	})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

// Exercise real process execution: credentials are inherited, failures propagate,
// and cancellation terminates a stalled CLI instead of reporting success.
func TestAWSHelperProcess(t *testing.T) {
	if os.Getenv("AWS_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "fail":
		os.Exit(7)
	case "wait":
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}
func TestExecuteAWSExitAndCancellation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CLI_BIN", binary)
	t.Setenv("AWS_HELPER_PROCESS", "1")
	if err := executeProcess(context.Background(), []string{"-test.run=TestAWSHelperProcess", "--", "fail"}, io.Discard); err == nil {
		t.Fatal("CLI exit failure lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := executeProcess(ctx, []string{"-test.run=TestAWSHelperProcess", "--", "wait"}, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout lost: %v", err)
	}
}

func TestEndpointSchemes(t *testing.T) {
	for _, endpoint := range []string{"http://192.168.1.10:8080", "http://s3.example.com/", "https://s3.example.com:9443"} {
		t.Run(endpoint, func(t *testing.T) {
			configEnv(t)
			t.Setenv("S3_ENDPOINT", endpoint)
			if _, err := loadConfig(); err != nil {
				t.Fatalf("valid endpoint rejected: %v", err)
			}
			// Verify the selected protocol and port reach the CLI unchanged.
			populatedSource(t)
			t.Setenv("S3_ENDPOINT", endpoint)
			err := runWithAWS(context.Background(), func(ctx context.Context, args []string, w io.Writer) error {
				for i, arg := range args {
					if arg == "--endpoint-url" && i+1 < len(args) && args[i+1] == endpoint {
						return nil
					}
				}
				t.Fatal("endpoint was not passed unchanged")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, endpoint := range []string{"ftp://s3.example.com", "s3.example.com", "http://s3.example.com/bucket", "http://user:secret@s3.example.com", "http://s3.example.com?key=value", "http://s3.example.com#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			configEnv(t)
			t.Setenv("S3_ENDPOINT", endpoint)
			if _, err := loadConfig(); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
}

func TestBackupCommandsNeverTargetLocalFiles(t *testing.T) {
	for _, deletion := range []string{"false", "true"} {
		t.Run(deletion, func(t *testing.T) {
			populatedSource(t)
			t.Setenv("BACKUP_DELETE", deletion)
			err := runWithAWS(context.Background(), func(_ context.Context, args []string, w io.Writer) error {
				if err := validateBackupCommand(args); err != nil {
					return err
				}
				if args[5] == "s3api" {
					_, err := io.WriteString(w, `{"Status":"Enabled"}`)
					return err
				}
				if args[7] != filepath.Clean(os.Getenv("BACKUP_SOURCE"))+string(os.PathSeparator) || args[8] != "s3://backup/web-01/www/" {
					t.Fatalf("unsafe sync direction: %v", args)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecuteAWSRejectsUnsafeCommandsBeforeStartingProcess(t *testing.T) {
	// An invalid executable proves rejection happens before any child process starts.
	t.Setenv("AWS_CLI_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	base := []string{"--no-cli-pager", "--endpoint-url", "http://s3.example.com", "--region", "us-east-1"}
	local := t.TempDir()
	for _, command := range [][]string{
		{"s3", "sync", "s3://backup/prefix/", local, "--delete"},
		{"s3", "sync", local, local, "--delete"},
		{"s3", "rm", local, "--recursive"},
		{"s3", "mv", local, "s3://backup/prefix/", "--recursive"},
		{"s3api", "delete-object", "--bucket", "backup", "--key", "file"},
		{"s3", "sync", local, "s3://backup/prefix/", "--no-follow-symlinks", "--exclude", ".s3-backup-source", "--no-progress", "--endpoint-url", "http://other"},
	} {
		err := executeAWS(context.Background(), append(append([]string{}, base...), command...), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unsafe backup command") {
			t.Fatalf("command %v was not blocked before execution: %v", command, err)
		}
	}
}
