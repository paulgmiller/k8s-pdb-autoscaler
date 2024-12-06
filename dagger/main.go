package main

import (
	"context"
	"log"
	"strings"

	"dagger.io/dagger"
)

func main() {
	ctx := context.Background()

	// Initialize Dagger client
	client, err := dagger.Connect(ctx)
	if err != nil {
		log.Fatalf("Failed to connect to Dagger: %v", err)
	}
	defer client.Close()

	// Get Git commit hash
	log.Println("Fetching the latest Git commit hash...")
	repo := client.Host().Directory(".")
	commitHash, err := client.Container().
		From("alpine/git").
		WithMountedDirectory("/repo", repo).
		WithWorkdir("/repo").
		WithExec([]string{"git", "rev-parse", "--short", "HEAD"}).
		Stdout(ctx)
	if err != nil {
		log.Fatalf("Failed to get Git commit hash: %v", err)
	}
	commitHash = strings.TrimSpace(commitHash)
	log.Printf("Using commit hash as tag: %s", commitHash)

	// Run `make test`
	log.Println("Running tests...")
	make := client.Container().
		From("ubuntu"). //too big but needs make
		WithMountedDirectory("/src", repo).
		WithWorkdir("/src").
		WithExec([]string{"make", "test"})

	_, err = make.Stdout(ctx)
	if err != nil {
		log.Fatalf("Tests failed: %v", err)
	}
	log.Println("Tests passed!")

	// Build Docker image
	log.Println("Building Docker image...")
	image := "your-registry/your-app:" + commitHash
	//dockerfile := repo.File("Dockerfile")
	imageRef, err := client.Container().
		Build(repo.Directory(".")).
		Publish(ctx, image)
	if err != nil {
		log.Fatalf("Failed to build Docker image: %v", err)
	}
	log.Printf("Docker image built and pushed: %s", imageRef)

	// Deploy to Kubernetes
	log.Println("Deploying to Kubernetes...")
	kubeConfig := client.Host().File("/path/to/kubeconfig")
	kubectl := client.Container().
		From("bitnami/kubectl").
		WithMountedFile("/root/.kube/config", kubeConfig).
		WithExec([]string{"kubectl", "set", "image", "deployment/your-deployment", "your-container=" + image})

	_, err = kubectl.Stdout(ctx)
	if err != nil {
		log.Fatalf("Failed to update deployment in Kubernetes: %v", err)
	}

	log.Println("Deployment complete!")
}
