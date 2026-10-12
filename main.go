package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxOutputSize = 512 * 1024 // 512KB
	maxSourceSize = 100 * 1024 // 100KB
	maxExecTime   = 10 * time.Second
	// A compile stage gets its own budget: erlc and elixirc are fast on their
	// own, but a large module set can still outlast the run limit, and a compile
	// killed at 10s would report a timeout as if it were the program's.
	maxCompileTime = 15 * time.Second
	workspaceDir   = "/tmp/codhoot-workspace"
)

// langSpec is one served language: the entry filename the backend's source maps
// to, the binaries it needs on PATH, and the two command lines the job runs.
//
// The compile and run commands are the backend registry's own CompileCommand and
// RunCommand for each language (internal/compiler/registry), carried here as
// templates so the shared service dispatches exactly the commands the local
// docker runner would have run inside codhoot-runner-systems. {src} expands to the
// absolute path of the written entry file; a Run with no {src} entry does not
// take the source path (some run commands name the loaded module instead).
type langSpec struct {
	Entry   string
	Look    []string
	Compile []string
	Run     []string
}

// langOrder fixes the boot-time toolchain check order so the log line is the
// same on every start; map iteration order is not.
var langOrder = []string{"ada", "assembly", "cobol", "d", "fortran", "nim", "pascal"}

var langSpecs = map[string]langSpec{
	"ada": {
		Entry:   "main.adb",
		Look:    []string{"gnatmake"},
		Compile: []string{"gnatmake", "-o", "main", "{src}"},
		Run:     []string{"./main"},
	},
	"assembly": {
		Entry: "main.asm",
		Look:  []string{"nasm", "ld"},
		// Assemble and link in one shell step: the runner's registry entry
		// needs both a .o and the final binary, and doing it in two separate
		// child processes would expose the intermediate file to a second
		// sandboxed exec that serves no purpose.
		Compile: []string{"sh", "-c", "nasm -f elf64 -o main.o {src} && ld -o main main.o"},
		Run:     []string{"./main"},
	},
	"cobol": {
		Entry:   "main.cob",
		Look:    []string{"cobc"},
		Compile: []string{"cobc", "-x", "-free", "-o", "main", "{src}"},
		Run:     []string{"./main"},
	},
	"d": {
		Entry:   "main.d",
		Look:    []string{"gdc"},
		Compile: []string{"gdc", "{src}", "-o", "main"},
		Run:     []string{"./main"},
	},
	"fortran": {
		Entry:   "main.f90",
		Look:    []string{"gfortran"},
		Compile: []string{"gfortran", "{src}", "-o", "main"},
		Run:     []string{"./main"},
	},
	"nim": {
		Entry:   "main.nim",
		Look:    []string{"nim"},
		Compile: []string{"nim", "c", "--hints:off", "-o:main", "{src}"},
		Run:     []string{"./main"},
	},
	"pascal": {
		Entry:   "main.pas",
		Look:    []string{"fpc"},
		Compile: []string{"fpc", "-omain", "{src}"},
		Run:     []string{"./main"},
	},
}

// extToLang resolves a language from the entry filename for callers that send
// no language field. The backend always sends one; this keeps a request written
// against the older single-language contract working.
var extToLang = map[string]string{
	".adb": "ada",
	".asm": "assembly",
	".cob": "cobol",
	".d":   "d",
	".f90": "fortran",
	".nim": "nim",
	".pas": "pascal",
}

func expandTemplate(tmpl []string, src string) []string {
	out := make([]string, len(tmpl))
	for i, part := range tmpl {
		out[i] = strings.ReplaceAll(part, "{src}", src)
	}
	return out
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return strings.ToLower(name[i:])
	}
	return ""
}

type CompileRequest struct {
	Source string `json:"source"`
	// Files plus EntryFile enable the multi-file contract the backend uses.
	// Source is still accepted so older callers keep working.
	Files     []File `json:"files,omitempty"`
	EntryFile string `json:"entry_file,omitempty"`
	// Language selects the toolchain. This service is shared by the BEAM
	// languages, so unlike the single-language services it cannot infer the
	// compiler from the entry filename alone.
	Language string `json:"language,omitempty"`
}

type CompileResponse struct {
	Success         bool   `json:"success"`
	Output          string `json:"output,omitempty"`
	Error           string `json:"error,omitempty"`
	ExitCode        int    `json:"exit_code"`
	CompileTime     int64  `json:"compile_time_ms"`
	ExecuteTime     int64  `json:"execute_time_ms"`
	Timeout         bool   `json:"timeout,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

type HealthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// maxConcurrentJobs bounds concurrent compiler+runner processes so a burst of
// students cannot OOM the 512MB free-tier container (which would restart it and
// wipe every warm cache). Requests queue up to their own deadline instead.
//
// The default is two, not the four the single-language services use: an Erlang
// or Elixir job holds a BEAM virtual machine, which allocates a scheduler and
// process heap eagerly whether or not the program needs them, so two of them
// plus the service itself is what 512 MB actually holds. MAX_CONCURRENT_JOBS
// overrides it per deployment, clamped to 1..8.
var maxConcurrentJobs = concurrentJobsFromEnv(2)

// concurrentJobsFromEnv reads MAX_CONCURRENT_JOBS, clamped to 1..8 so a typo
// cannot silently remove the ceiling.
func concurrentJobsFromEnv(def int) int {
	n, err := strconv.Atoi(os.Getenv("MAX_CONCURRENT_JOBS"))
	if err != nil || n < 1 {
		return def
	}
	if n > 8 {
		return 8
	}
	return n
}

var jobSem = make(chan struct{}, maxConcurrentJobs)

// uids hands each in-flight job its own sandbox uid so a job's working directory
// is genuinely private to it. See harden.go for why a single shared uid is not
// isolation.
var uids = newUIDPool()

func main() {
	// Sandboxed path: when re-invoked as the trampoline, apply rlimits and exec
	// the real compiler or runtime. Returns only if the trampoline fails.
	if sandboxExec() {
		return
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	os.MkdirAll(workspaceDir, 0755)
	defer os.RemoveAll(workspaceDir)

	// Every served language's toolchain must be present before the port opens:
	// a service that accepts /compile and then fails LookPath on the first job
	// looks like a language bug to the user, not a broken image.
	found := make([]string, 0, len(langOrder))
	for _, lang := range langOrder {
		spec := langSpecs[lang]
		for _, bin := range spec.Look {
			if _, err := exec.LookPath(bin); err != nil {
				log.Fatalf("%s not found (%s needs it): %v", bin, lang, err)
			}
			found = append(found, bin)
		}
	}
	log.Printf("systems toolchains found (%s), ready to execute", strings.Join(found, ", "))

	mux := http.NewServeMux()
	// Only POST /compile is authenticated. /health and /health/live must stay
	// open or Render marks the service unhealthy and restarts it in a loop.
	mux.Handle("POST /compile", VerifyHMAC(http.HandlerFunc(handleCompile)))
	mux.HandleFunc("GET /health/live", handleHealth)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /", handleIndex)

	handler := corsMiddleware(loggingMiddleware(mux))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: maxExecTime + 20*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("BEAM Compiler Service listening on :%s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"service": "codhoot-systems-compiler",
		"version": "2.0.0",
		"usage":   "POST /compile with {\"language\": \"erlang|elixir\", \"source\": \"...\"}",
	})
}

// runCommand runs a command, killing the whole process group on timeout so
// orphaned children never accumulate on the 512MB container.
//
// Every child is confined to uid's private job directory, run with a fixed
// minimal environment, capped by rlimits, and its output is captured through a
// bounded writer so a runaway program cannot exhaust the service's memory.
func runCommand(parent context.Context, timeout time.Duration, dir string, uid uint32, name string, args ...string) ([]byte, int, bool) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	self, err := selfPath()
	if err != nil {
		return nil, -1, false
	}

	// Run the sandbox trampoline (this same binary), which applies rlimits and
	// then execs the real target. That is how rlimits get applied at all, since
	// Go's os/exec cannot set them in a child.
	full := append([]string{sandboxExecFlag, name}, args...)

	cmd := exec.CommandContext(ctx, self, full...)
	cmd.Dir = dir
	cmd.Env = childEnv(dir)
	cmd.SysProcAttr = hardenProcAttr(uid)

	killGroup := func() {
		for i := 0; i < 200 && (cmd.Process == nil || cmd.Process.Pid <= 0); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			killProcessGroup(cmd.Process.Pid)
		}
	}
	go func() {
		<-ctx.Done()
		killGroup()
	}()

	cw := &cappedWriter{max: maxOutputSize}
	cmd.Stdout = cw
	cmd.Stderr = cw
	runErr := cmd.Run()

	out := cw.Bytes()
	if ctx.Err() == context.DeadlineExceeded {
		return out, -1, true
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return out, ee.ExitCode(), false
		}
		return out, 1, false
	}
	return out, 0, false
}

func handleCompile(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req CompileRequest
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", 0, 0)
		return
	}

	// A multi-file request carries no source field; resolveEntry validates the
	// file set instead, so these single-source checks apply only when it is absent.
	if len(req.Files) == 0 {
		if strings.TrimSpace(req.Source) == "" {
			writeError(w, http.StatusBadRequest, "Source code is required", 0, 0)
			return
		}
		if len(req.Source) > maxSourceSize {
			writeError(w, http.StatusBadRequest, "Source code exceeds maximum size", 0, 0)
			return
		}
	}

	// Resolve the toolchain before any work is done. A request for a language
	// this image does not serve is a routing mistake, and saying so plainly beats
	// running the erlang compiler over an Elixir program.
	lang := strings.TrimSpace(req.Language)
	if lang == "" && req.EntryFile != "" {
		lang = extToLang[extOf(req.EntryFile)]
	}
	spec, ok := langSpecs[lang]
	if !ok {
		writeError(w, http.StatusBadRequest,
			"codhoot-beam-service runs erlang and elixir only (language "+strconv.Quote(lang)+" is not served here)", 0, 0)
		return
	}

	select {
	case jobSem <- struct{}{}:
		defer func() { <-jobSem }()
	case <-r.Context().Done():
		writeError(w, http.StatusServiceUnavailable, "Compiler is busy, try again", 0, 0)
		return
	}

	// Each job gets its own uid and a private random 0700 directory. Sharing one
	// predictable directory let concurrent jobs read each other's source and
	// rewrite it before it was executed.
	uid := uids.acquire()
	defer uids.release(uid)

	jobDir, err := newJobDir(uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create workspace", 0, 0)
		return
	}
	defer os.RemoveAll(jobDir)

	ep, err := resolveEntry(req.Source, req.Files, req.EntryFile, spec.Entry)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), 0, 0)
		return
	}
	srcFile, werr := writeEntry(jobDir, ep, uid)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "Failed to write source", 0, 0)
		return
	}
	// The file was created by the service (root); give it to the sandbox uid so
	// the unprivileged interpreter can read it and nothing else can.
	if err := os.Chown(srcFile, int(uid), int(uid)); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to write source", 0, 0)
		return
	}

	// Compiled languages get their bytecode built first, in the same private
	// directory and under the same uid as the run. A failed compile is reported
	// with the compiler's own diagnostics as the output, which is what the user
	// needs to see; the run stage is not reached.
	var compileMs int64
	if spec.Compile != nil {
		args := expandTemplate(spec.Compile, srcFile)
		compileStart := time.Now()
		cOut, cCode, cTimeout := runCommand(context.Background(), maxCompileTime, jobDir, uid, args[0], args[1:]...)
		compileMs = time.Since(compileStart).Milliseconds()

		cTruncated := len(cOut) >= maxOutputSize
		if cTruncated {
			cOut = cOut[:maxOutputSize]
		}
		if cTimeout || cCode != 0 {
			resp := CompileResponse{
				Output:          sanitizeOutput(string(cOut), jobDir),
				ExitCode:        cCode,
				CompileTime:     compileMs,
				Timeout:         cTimeout,
				OutputTruncated: cTruncated,
			}
			if cTimeout {
				resp.Error = "Compilation timed out (limit: 15s)"
			} else if resp.Output == "" {
				resp.Error = "Compilation failed"
			}
			respond(w, resp)
			log.Printf("%s: compile failed exit=%d compile=%dms", lang, cCode, compileMs)
			return
		}
	}

	runArgs := expandTemplate(spec.Run, srcFile)
	execStart := time.Now()
	output, exitCode, timedOut := runCommand(context.Background(), maxExecTime, jobDir, uid, runArgs[0], runArgs[1:]...)
	execMs := time.Since(execStart).Milliseconds()

	truncated := false
	if len(output) >= maxOutputSize {
		output = output[:maxOutputSize]
		truncated = true
	}

	// Strip terminal control sequences and internal paths before the client sees it.
	clean := sanitizeOutput(string(output), jobDir)
	resp := CompileResponse{
		Success:         exitCode == 0,
		Output:          clean,
		ExitCode:        exitCode,
		CompileTime:     compileMs,
		ExecuteTime:     execMs,
		Timeout:         timedOut,
		OutputTruncated: truncated,
	}
	if timedOut {
		resp.Error = "Execution timed out (limit: 10s)"
	} else if exitCode != 0 && resp.Output == "" {
		resp.Error = "Execution failed"
	}

	respond(w, resp)

	log.Printf("%s: exit=%d compile=%dms exec=%dms total=%dms timeout=%v",
		lang, exitCode, compileMs, execMs, time.Since(start).Milliseconds(), timedOut)
}

// respond writes the compile response. The status is always 200: whether the
// user's program passed is carried in the body, and a non-200 here would read as
// "the service is broken" to the backend's warm-up retry logic.
func respond(w http.ResponseWriter, resp CompileResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, status int, message string, compileMs, execMs int64) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(CompileResponse{
		Success:     false,
		Error:       message,
		ExitCode:    -1,
		CompileTime: compileMs,
		ExecuteTime: execMs,
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
