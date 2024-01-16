package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dagger.io/dagger"
)

var hooksPath = flag.String("hooks", "", "path to an optional hooks.json file. If not provided it will start in gitCloneProxy mode")

func main() {
	flag.Parse()

	ctx := context.Background()
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		log.Fatalf("failed to connect to dagger: %v", err)
	}
	defer client.Close()

	// we're warming up the webhook container here so we don't make the
	// user wait for the first request to be served
	if _, err = webhookContainer(client).Sync(ctx); err != nil {
		log.Fatalf("failed to build webhook: %v", err)
	}

	mux := http.NewServeMux()

	stop := func() {}
	if *hooksPath != "" {
		log.Println("starting reverse proxy mode")

		hooksFile := client.Host().File(*hooksPath)
		hooks, err := hooksFile.Contents(ctx)
		if err != nil {
			log.Fatalf("failed to read hooks file: %v", err)
		}
		fmt.Println(hooks)
		if hooks == "" {
			log.Fatalf("hooks file is empty")
		}

		// override stop function to stop the reverse proxy
		var handler http.HandlerFunc
		stop, handler = reverseProxy(ctx, client, hooksFile)

		mux.HandleFunc("/", handler)
	} else {
		log.Println("starting git proxy mode")

		mux.HandleFunc("/", gitCloneProxy())
	}

	fmt.Println("starting proxy server in port 8080")
	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	_, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		log.Println("stopping reverse proxy if there is any")
		stop()
		log.Println("received sigint, cancelling context")
		cancel()
	}()

	err = srv.ListenAndServe()
	if err != nil {
		log.Printf("serve exited with: %v", err)
	}
}

// TODO(matipan): clean this up to reuse the init of the service and tunnel
// both here and in the gitCloneProxy
func reverseProxy(ctx context.Context, client *dagger.Client, hooks *dagger.File) (stop func(), handler http.HandlerFunc) {
	svc := webhookContainer(client).
		WithFile("/hooks/hooks.json", hooks).
		WithWorkdir("/hooks").
		WithExposedPort(9000).
		WithExec([]string{"-verbose", "-port", "9000", "-hooks", "hooks.json"}, dagger.ContainerWithExecOpts{ExperimentalPrivilegedNesting: true}).
		AsService()

	tunnel, err := client.Host().Tunnel(svc).Start(ctx)
	if err != nil {
		log.Fatalf("failed to start webhook container: %s", err)
	}

	endpoint, err := tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{Scheme: "http"})
	if err != nil {
		log.Fatalf("failed to obtain service endpoint: %s", err)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		log.Fatalf("failed to parse endpoint: %s", err)
	}

	rp := httputil.NewSingleHostReverseProxy(u)

	return func() {
			svc.Stop(ctx)
			tunnel.Stop(ctx)
		}, func(w http.ResponseWriter, r *http.Request) {
			log.Printf("proxying request to %s", endpoint)
			rp.ServeHTTP(w, r)
		}
}

type GithubWebhook struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	After string `json:"after"`
}

// gitCloneProxy returns a handler that will first clone a git repository into
// the specified directory and then proxy the request to the reverse proxy.
func gitCloneProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("failed to get request body: %s", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewBuffer(b))

		gh := &GithubWebhook{}
		if err = json.Unmarshal(b, gh); err != nil {
			log.Printf("failed to decode JSON payload: %s", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		ctx := r.Context()
		client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
		if err != nil {
			log.Printf("fail to connect to dagger client: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer client.Close()

		repo := client.Git("https://github.com/" + gh.Repository.FullName).Commit(gh.After).Tree()
		fullRepo := strings.Split(gh.Repository.FullName, "/")
		repoName := fullRepo[len(fullRepo)-1]
		svc := webhookContainer(client).
			WithDirectory("/"+repoName, repo).
			WithWorkdir("/"+repoName).
			WithExposedPort(9000).
			WithExec([]string{"-verbose", "-port", "9000", "-hooks", "hooks.json"}, dagger.ContainerWithExecOpts{ExperimentalPrivilegedNesting: true}).
			AsService()

		tunnel, err := client.Host().Tunnel(svc).Start(ctx)
		if err != nil {
			log.Printf("failed to start webhook container: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() {
			log.Println("stopping service")
			svc.Stop(ctx)
			log.Println("stopping tunnel")
			tunnel.Stop(ctx)
		}()

		endpoint, err := tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{Scheme: "http"})
		if err != nil {
			log.Printf("failed to obtain service endpoint: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		u, err := url.Parse(endpoint)
		if err != nil {
			log.Printf("failed to parse endpoint: %s", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("proxying request to %s", endpoint)

		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	}
}

func webhookContainer(c *dagger.Client) *dagger.Container {
	// TODO download the right binary for $PLATFORM/$ARCH
	return c.Container().From("ubuntu:lunar").
		WithExec([]string{"sh", "-c", "apt update && apt install -y wget"}).
		WithExec([]string{"wget", "-q", "https://github.com/adnanh/webhook/releases/download/2.8.1/webhook-linux-amd64.tar.gz"}).
		WithExec([]string{"tar", "-C", "/usr/local/bin", "--strip-components", "1", "-xf", "webhook-linux-amd64.tar.gz", "webhook-linux-amd64/webhook"}).
		WithEntrypoint([]string{"/usr/local/bin/webhook"})
}
