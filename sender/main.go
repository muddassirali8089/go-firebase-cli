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
	if len(os.Args) < 2 {
		fmt.Println("Usage: sender <command>")
		fmt.Println("Example: sender dir")
		fmt.Println("Example: sender ipconfig")
		os.Exit(1)
	}

	// Join all arguments so you can send things like: sender echo hello world
	cmdStr := strings.Join(os.Args[1:], " ")
	ctx := context.Background()

	// Connect to Firebase (reads the key from the parent folder)
	credPath := "../serviceAccountKey.json"
	if envPath := os.Getenv("FIREBASE_CREDENTIALS"); envPath != "" {
		credPath = envPath
	}

	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(credPath))
	if err != nil {
		log.Fatalf("❌ Failed to init Firebase: %v", err)
	}
	fsClient, err := app.Firestore(ctx)
	if err != nil {
		log.Fatalf("❌ Failed to get Firestore client: %v", err)
	}
	defer fsClient.Close()

	// Create the new command document
	doc := map[string]interface{}{
		"cmd":     cmdStr,
		"pending": true,
		"results": "",
	}

	ref, _, err := fsClient.Collection("commands").Add(ctx, doc)
	if err != nil {
		log.Fatalf("❌ Failed to send command: %v", err)
	}

	fmt.Printf("🚀 Command sent to Firebase!\n")
	fmt.Printf("   Command: %s\n", cmdStr)
	fmt.Printf("   Doc ID:  %s\n", ref.ID)
	fmt.Printf("   Wait up to 10 seconds for the listener to pick it up.\n")
}