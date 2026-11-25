package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"
	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"
)

const (
	collectionName = "commands"
	pollInterval   = 10 * time.Second
	cmdTimeout     = 60 * time.Second
)

// 🪄 THIS IS THE MAGIC LINE.
// It embeds the JSON file directly into the .exe at compile time.
//go:embed serviceAccountKey.json
var credBytes []byte

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown on Ctrl+C
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n🛑 Shutting down...")
		cancel()
	}()

	// 1. Connect to Firebase using the EMBEDDED bytes
	creds := option.WithCredentialsJSON(credBytes)
	app, err := firebase.NewApp(ctx, nil, creds)
	if err != nil {
		handleFatalError("❌ Failed to init Firebase", err)
	}
	
	fsClient, err := app.Firestore(ctx)
	if err != nil {
		handleFatalError("❌ Failed to get Firestore client", err)
	}
	defer fsClient.Close()

	fmt.Println("✅ Connected to Firebase")
	fmt.Printf("👀 Watching collection '%s' every %s (Ctrl+C to stop)\n", collectionName, pollInterval)

	// 2. Start polling
	runCycle(ctx, fsClient)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("👋 Bye.")
			return
		case <-ticker.C:
			runCycle(ctx, fsClient)
		}
	}
}

func runCycle(ctx context.Context, client *firestore.Client) {
	fmt.Println("🔍 Checking for pending commands...")

	iter := client.Collection(collectionName).Where("pending", "==", true).Documents(ctx)
	defer iter.Stop()

	found := 0
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		found++

		data := doc.Data()
		cmdStr, _ := data["cmd"].(string)
		if cmdStr == "" {
			continue
		}

		fmt.Printf("▶️  [%s] Running: %s\n", doc.Ref.ID, cmdStr)
		result, runErr := runShell(cmdStr)
		if runErr != nil {
			result = fmt.Sprintf("[error] %v\n%s", runErr, result)
		}

		_, err = doc.Ref.Update(ctx, []firestore.Update{
			{Path: "pending", Value: false},
			{Path: "results", Value: result},
		})
		if err != nil {
			fmt.Printf("⚠️  Failed to save result for %s: %v\n", doc.Ref.ID, err)
		} else {
			fmt.Printf("✅ Saved result for %s\n", doc.Ref.ID)
		}
	}

	if found == 0 {
		fmt.Println("   (no pending commands)")
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

// handleFatalError prints the error and waits for the user to press Enter
// so the window doesn't close instantly on double-click.
func handleFatalError(msg string, err error) {
	fmt.Printf("\n%s: %v\n\n", msg, err)
	fmt.Println("Press Enter to exit...")
	fmt.Scanln()
	os.Exit(1)
}