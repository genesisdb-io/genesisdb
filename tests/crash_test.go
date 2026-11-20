package tests

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The crash test runs the real server binary, commits over HTTP from many
// concurrent clients and stops the server in the middle of writing: with
// SIGKILL, with a graceful SIGTERM, or with a simulated power loss. After
// every stop it verifies the stopped directory offline, restarts the server
// and compares the store with what every client was told, see
// crash_load_test.go.
//
// It is opt-in because it takes minutes and starts processes:
//
//	GENESISDB_CRASH_TEST=5m go test -count=1 -run TestCrash -v -timeout 15m ./tests
//
// Optional settings:
//
//	GENESISDB_CRASH_WRITERS          concurrent clients, default 48
//	GENESISDB_CRASH_DIRECTORY        parent of the disposable data directory,
//	                                 default a temporary directory
//	GENESISDB_CRASH_BINARY           server binary, default "go build" of
//	                                 cmd/genesisdb (use a -race build to let
//	                                 the server report data races)
//	GENESISDB_CRASH_CUT_COMMAND      shell command that cuts the power of the
//	                                 volume: every later write is lost
//	GENESISDB_CRASH_RESTORE_COMMAND  shell command that brings the volume
//	                                 back after the server is gone
//
// The two commands are not part of the repository. A device mapper volume
// that is switched to the "error" target and back is one way to provide them.
const (
	crashToken           = "crash-test-administrator-token-that-is-not-a-secret"
	crashEventsPerStore  = 30000
	crashGracefulEvery   = 5
	crashStartupDeadline = 60 * time.Second
)

// crashHarness owns one server process and the expectations of every client.
type crashHarness struct {
	t             *testing.T
	binary        string
	workDirectory string
	dataDirectory string
	baseURL       string
	environment   []string
	client        *http.Client
	cutCommand    string
	restore       string

	mutex    sync.Mutex
	records  []*crashRecord
	subjects map[int][]string
	erasures map[string]erasureState

	totals crashTotals
}

type crashTotals struct {
	kills, gracefulStops, powerCuts int
	acknowledged, verifiedEvents    int
	acknowledgedInEarlierStores     int
	tornLogTails                    int
	erasures, maintenanceCalls      int
	stores                          int
}

func TestCrash_AcknowledgedCommitsSurviveEveryStop(t *testing.T) {
	setting := os.Getenv("GENESISDB_CRASH_TEST")
	if setting == "" {
		t.Skip("set GENESISDB_CRASH_TEST to a duration (for example 5m) to run the crash test")
	}
	duration, err := time.ParseDuration(setting)
	if err != nil || duration <= 0 {
		t.Fatalf("GENESISDB_CRASH_TEST must be a duration like 5m, got %q", setting)
	}
	writers := 48
	if text := os.Getenv("GENESISDB_CRASH_WRITERS"); text != "" {
		if writers, err = strconv.Atoi(text); err != nil || writers < 2 {
			t.Fatalf("GENESISDB_CRASH_WRITERS must be a number of at least 2, got %q", text)
		}
	}
	harness := newCrashHarness(t)
	random := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	deadline := time.Now().Add(duration)
	for round := 1; time.Now().Before(deadline) && !t.Failed(); round++ {
		server := harness.startServer()
		events := harness.verify(round)
		if t.Failed() {
			harness.stopGracefully(server)
			break
		}
		if events >= crashEventsPerStore {
			// A fresh store keeps every verification bounded.
			harness.stopGracefully(server)
			harness.startNewStore()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		var group sync.WaitGroup
		for writer := 0; writer < writers; writer++ {
			group.Add(1)
			go func() {
				defer group.Done()
				harness.runWriter(ctx, round, writer)
			}()
		}
		group.Add(1)
		go func() {
			defer group.Done()
			harness.runMaintenance(ctx)
		}()
		time.Sleep(time.Duration(300+random.Intn(1200)) * time.Millisecond)
		switch {
		case harness.cutCommand != "":
			harness.runCommand(harness.cutCommand)
			time.Sleep(100 * time.Millisecond)
			harness.kill(server)
			harness.totals.powerCuts++
		case round%crashGracefulEvery == 0:
			harness.stopGracefully(server)
			harness.totals.gracefulStops++
		default:
			harness.kill(server)
			harness.totals.kills++
		}
		cancel()
		group.Wait()
		harness.client.CloseIdleConnections()
		if harness.restore != "" {
			harness.runCommand(harness.restore)
		}
		harness.verifyOffline(round)
	}
	if !t.Failed() {
		server := harness.startServer()
		events := harness.verify(-1)
		status, content, err := harness.call(context.Background(), http.MethodPost, "/api/v2/admin/index", "")
		if err != nil || status != http.StatusOK {
			t.Errorf("index rebuild: %d %v %s", status, err, content)
		}
		if rebuilt := harness.verify(-2); rebuilt != events {
			t.Errorf("index rebuild changed the store: %d events before, %d after", events, rebuilt)
		}
		harness.stopGracefully(server)
	}
	harness.checkServerLog()
	totals := harness.totals
	t.Logf("stops: %d kills, %d graceful, %d power cuts (%d left an unterminated record for recovery). %d commits acknowledged, %d events verified in %d stores, %d erasures, %d audits and backups under load",
		totals.kills, totals.gracefulStops, totals.powerCuts, totals.tornLogTails, totals.acknowledged, totals.verifiedEvents, totals.stores+1, totals.erasures, totals.maintenanceCalls)
}

func newCrashHarness(t *testing.T) *crashHarness {
	t.Helper()
	harness := &crashHarness{
		t:             t,
		workDirectory: t.TempDir(),
		client:        &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 512}},
		cutCommand:    os.Getenv("GENESISDB_CRASH_CUT_COMMAND"),
		restore:       os.Getenv("GENESISDB_CRASH_RESTORE_COMMAND"),
		subjects:      map[int][]string{},
		erasures:      map[string]erasureState{},
	}
	if (harness.cutCommand == "") != (harness.restore == "") {
		t.Fatal("GENESISDB_CRASH_CUT_COMMAND and GENESISDB_CRASH_RESTORE_COMMAND must be set together")
	}
	// The data directory is always a new directory created here, never a
	// directory that existed before.
	parent := os.Getenv("GENESISDB_CRASH_DIRECTORY")
	if parent == "" {
		parent = t.TempDir()
	}
	dataDirectory, err := os.MkdirTemp(parent, "genesisdb-crash-test-")
	if err != nil {
		t.Fatal(err)
	}
	harness.dataDirectory = dataDirectory
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	// The new directory entry must survive the first power cut.
	if directory, err := os.Open(parent); err == nil {
		directory.Sync()
		directory.Close()
	}

	harness.binary = os.Getenv("GENESISDB_CRASH_BINARY")
	if harness.binary == "" {
		harness.binary = filepath.Join(harness.workDirectory, "genesisdb")
		build := exec.Command("go", "build", "-o", harness.binary, "./cmd/genesisdb")
		build.Dir = repositoryRoot
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build the server: %v\n%s", err, output)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	harness.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	// A key generated for this run only: every event is signed, so the
	// audit also proves that no group lost or mixed up a signature.
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	harness.environment = []string{
		"GENESISDB_AUTH_TOKEN=" + crashToken,
		"GENESISDB_HOST=127.0.0.1",
		"GENESISDB_PORT=" + strconv.Itoa(port),
		"GENESISDB_DATA_DIR=" + dataDirectory,
		"GENESISDB_LOG_LEVEL=warn",
		"GENESISDB_SIGNING_KEY=" + base64.StdEncoding.EncodeToString(encodedKey),
		"HOME=" + harness.workDirectory,
		"PATH=" + os.Getenv("PATH"),
	}
	return harness
}

func (harness *crashHarness) serverLogPath() string {
	return filepath.Join(harness.workDirectory, "server.log")
}

// crashServer owns the single waiter for a started server process.
// Every shutdown path waits for exited rather than calling Wait again.
type crashServer struct {
	command *exec.Cmd
	exited  <-chan struct{}
}

func watchCrashServer(command *exec.Cmd) *crashServer {
	exited := make(chan struct{})
	go func() {
		command.Wait()
		close(exited)
	}()
	return &crashServer{command: command, exited: exited}
}

// startServer starts the server and waits until it answers. Recovery of the
// previous stop happens here, before the first request is served.
func (harness *crashHarness) startServer() *crashServer {
	harness.t.Helper()
	logFile, err := os.OpenFile(harness.serverLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		harness.t.Fatal(err)
	}
	defer logFile.Close()
	server := exec.Command(harness.binary)
	// The working directory is empty, so no local .env file is read.
	server.Dir = harness.workDirectory
	server.Env = harness.environment
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		harness.t.Fatalf("start the server: %v", err)
	}
	running := watchCrashServer(server)
	deadline := time.Now().Add(crashStartupDeadline)
	for time.Now().Before(deadline) {
		select {
		case <-running.exited:
			harness.t.Fatalf("the server refused to start after a stop:\n%s", harness.serverLogTail())
		default:
		}
		if status, _, err := harness.call(context.Background(), http.MethodGet, "/api/v2/ping", ""); err == nil && status == http.StatusOK {
			return running
		}
		time.Sleep(20 * time.Millisecond)
	}
	harness.kill(running)
	harness.t.Fatalf("the server did not answer within %v:\n%s", crashStartupDeadline, harness.serverLogTail())
	return nil
}

func (harness *crashHarness) kill(server *crashServer) {
	server.command.Process.Kill()
	<-server.exited
}

// stopGracefully sends SIGTERM and falls back to SIGKILL.
func (harness *crashHarness) stopGracefully(server *crashServer) {
	server.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-server.exited:
	case <-time.After(30 * time.Second):
		harness.kill(server)
		harness.t.Errorf("the server did not stop within 30 seconds of SIGTERM")
	}
}

// startNewStore replaces the data directory content with an empty store and
// forgets the expectations of the old one, which were verified before.
func (harness *crashHarness) startNewStore() {
	entries, err := os.ReadDir(harness.dataDirectory)
	if err != nil {
		harness.t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(harness.dataDirectory, entry.Name())); err != nil {
			harness.t.Fatal(err)
		}
	}
	harness.mutex.Lock()
	harness.totals.acknowledgedInEarlierStores = harness.totals.acknowledged
	harness.records = nil
	harness.subjects = map[int][]string{}
	harness.erasures = map[string]erasureState{}
	harness.mutex.Unlock()
	harness.totals.stores++
}

func (harness *crashHarness) runCommand(command string) {
	harness.t.Helper()
	if output, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
		harness.t.Fatalf("%s: %v\n%s", command, err, output)
	}
}

// verifyOffline checks hash chain and signatures of the stopped directory
// with the offline verifier, before any recovery has run.
//
// A stop can leave an unterminated final record in a log. That is a
// legitimate state: the write was never acknowledged, and the next start
// removes it. The verifier refuses such a directory, which is counted, not
// reported. Whether anything acknowledged is missing is decided after the
// restart.
func (harness *crashHarness) verifyOffline(round int) {
	verify := exec.Command(harness.binary, "verify", "--data-dir", harness.dataDirectory)
	verify.Dir = harness.workDirectory
	verify.Env = []string{"HOME=" + harness.workDirectory, "PATH=" + os.Getenv("PATH")}
	output, err := verify.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(string(output), "incomplete record") {
		harness.totals.tornLogTails++
		return
	}
	harness.t.Errorf("round %d: offline verification of the stopped directory failed: %v\n%s", round, err, output)
}

func (harness *crashHarness) call(ctx context.Context, method, path, body string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, harness.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+crashToken)
	request.Header.Set("X-Disable-Ndjson", "1")
	response, err := harness.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	return response.StatusCode, content, err
}

func (harness *crashHarness) serverLogTail() string {
	content, err := os.ReadFile(harness.serverLogPath())
	if err != nil {
		return err.Error()
	}
	if len(content) > 4000 {
		content = content[len(content)-4000:]
	}
	return string(content)
}

// checkServerLog fails on a panic or, with a -race build of the server, on
// a reported data race.
func (harness *crashHarness) checkServerLog() {
	content, err := os.ReadFile(harness.serverLogPath())
	if err != nil {
		harness.t.Errorf("read the server log: %v", err)
		return
	}
	for _, problem := range []string{"WARNING: DATA RACE", "panic:", "fatal error:"} {
		if position := strings.Index(string(content), problem); position >= 0 {
			end := min(len(content), position+3000)
			harness.t.Errorf("the server reported %q:\n%s", problem, content[position:end])
		}
	}
}
