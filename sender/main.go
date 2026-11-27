package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: sender <TARGET_SYSTEM_ID> <command>")
		fmt.Println("Example: sender desktop-123_muddassir dir")
		fmt.Println("Example: sender desktop-123_muddassir screenshot")
		os.Exit(1)
	}

	targetSystem := os.Args[1]
	cmdStr := strings.Join(os.Args[2:], " ")
	ctx := context.Background()

	credPath := "../serviceAccountKey.json"
	if envPath := os.Getenv("FIREBASE_CREDENTIALS"); envPath != "" {
		credPath = envPath
	}

	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(credPath))
	if err != nil { log.Fatalf("❌ Failed to init Firebase: %v", err) }
	fsClient, err := app.Firestore(ctx)
	if err != nil { log.Fatalf("❌ Failed to get Firestore client: %v", err) }
	defer fsClient.Close()

	// Create the new command document with the specific target
	doc := map[string]interface{}{
		"cmd":     cmdStr,
		"target":  targetSystem,
		"pending": true,
		"results": "",
	}

	ref, _, err := fsClient.Collection("commands").Add(ctx, doc)
	if err != nil { log.Fatalf("❌ Failed to send command: %v", err) }

	fmt.Printf("🚀 Command sent to Firebase!\n")
	fmt.Printf("   Target:  %s\n", targetSystem)
	fmt.Printf("   Command: %s\n", cmdStr)
	fmt.Printf("   Doc ID:  %s\n", ref.ID)
}