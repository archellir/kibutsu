package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"kibutsu/api/handlers"
)

//go:embed frontend/build/*
var staticFiles embed.FS

func GetFileSystem() http.FileSystem {
	fsys, err := fs.Sub(staticFiles, "frontend/build")
	if err != nil {
		panic(err)
	}
	return http.FS(fsys)
}

type HealthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

type App struct {
	dockerClient *client.Client
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

type contextKey string

const requestIDKey contextKey = "requestID"

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		log.Printf(
			"[%s] %s %s %d %s",
			r.Context().Value(requestIDKey),
			r.Method,
			r.URL.Path,
			rw.status,
			time.Since(start),
		)
	})
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[PANIC] %v\n%s", err, debug.Stack())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func timeoutMiddleware(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5173")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-ID")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (app *App) dockerInfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	info, err := app.dockerClient.Info(ctx)
	if err != nil {
		http.Error(w, "Failed to get Docker info: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func main() {
	log.Println("Starting Docker management service...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	if _, err := dockerClient.Ping(ctx); err != nil {
		log.Fatalf("Failed to connect to Docker daemon: %v", err)
	}
	log.Println("Successfully connected to Docker daemon")

	app := &App{dockerClient: dockerClient}
	containerHandler := handlers.NewContainerHandler(dockerClient)
	imageHandler := handlers.NewImageHandler(dockerClient)
	composeHandler := handlers.NewComposeHandler(dockerClient)

	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", app.healthHandler)

	// API routes
	apiRouter := http.NewServeMux()
	apiRouter.HandleFunc("/docker/info", app.dockerInfoHandler)

	// Container endpoints
	apiRouter.HandleFunc("/containers", containerHandler.ListContainers)
	apiRouter.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/containers/")
		parts := strings.Split(path, "/")

		if len(parts) < 2 {
			containerHandler.GetContainer(w, r)
			return
		}

		switch parts[1] {
		case "start":
			containerHandler.StartContainer(w, r)
		case "stop":
			containerHandler.StopContainer(w, r)
		case "restart":
			containerHandler.RestartContainer(w, r)
		case "logs":
			containerHandler.GetContainerLogs(w, r)
		case "stats":
			containerHandler.GetContainerStats(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Image endpoints
	apiRouter.HandleFunc("/images", imageHandler.ListImages)
	apiRouter.HandleFunc("/images/pull", imageHandler.PullImage)
	apiRouter.HandleFunc("/system/info", imageHandler.GetSystemInfo)
	apiRouter.HandleFunc("/system/version", imageHandler.GetSystemVersion)
	apiRouter.HandleFunc("/system/disk", imageHandler.GetDiskUsage)
	apiRouter.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/history") {
			imageHandler.GetImageHistory(w, r)
			return
		}

		switch r.Method {
		case http.MethodGet:
			imageHandler.GetImage(w, r)
		case http.MethodDelete:
			imageHandler.RemoveImage(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Compose endpoints
	apiRouter.HandleFunc("/compose/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/compose/projects/")
		parts := strings.Split(path, "/")

		// If no project is specified, list projects.
		if path == "" || parts[0] == "" {
			if r.Method == http.MethodGet {
				composeHandler.ListProjects(w, r)
				return
			}
		}

		// If only the project name is provided, return project details.
		if len(parts) == 1 {
			if r.Method == http.MethodGet {
				composeHandler.GetProject(w, r)
				return
			}
		}

		// Otherwise route based on an action provided in the URL.
		projectName := parts[0]
		action := parts[1]
		log.Printf("Project: %s, Action: %s", projectName, action)
		switch action {
		case "up":
			if r.Method == http.MethodPost {
				composeHandler.ProjectUp(w, r)
				return
			}
		case "down":
			if r.Method == http.MethodPost {
				composeHandler.ProjectDown(w, r)
				return
			}
		case "logs":
			if r.Method == http.MethodGet {
				composeHandler.GetProjectLogs(w, r)
				return
			}
		case "services":
			// GET /compose/projects/{project}/services to list service details.
			if len(parts) == 2 && r.Method == http.MethodGet {
				composeHandler.ListServices(w, r)
				return
			} else if len(parts) == 4 && parts[3] == "scale" && r.Method == http.MethodPost {
				// Expected URL: /compose/projects/{project}/services/{service}/scale
				composeHandler.ScaleService(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})

	// Mount API router under /api
	mux.Handle("/api/", http.StripPrefix("/api", apiRouter))

	// Serve static files
	fileServer := http.FileServer(GetFileSystem())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets/") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	}))

	// Apply middleware chain
	handler := corsMiddleware(
		requestIDMiddleware(
			recoveryMiddleware(
				loggingMiddleware(
					timeoutMiddleware(30 * time.Second)(mux),
				),
			),
		),
	)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited properly")
}
