package chrometea

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/gagliardetto/utilz"
	"github.com/mafredri/cdp"
	"github.com/michenriksen/aquatone/agents"
)

func NewReadyBrowser() (*Browser, context.CancelFunc, error) {
	ctx := context.TODO()

	//////////////////////////////////////////////////
	// for updates, see https://github.com/chromedp/chromedp/blob/master/allocate.go
	// TODO: check what flags puppetteer does use.
	allocationOptions := []ExecAllocatorOption{
		Flag("ignore-certificate-errors", true),
		Flag("disable-notifications", true),
		Flag("disable-crash-reporter", true),
		Flag("incognito", true),
		Flag("disable-infobars", true),
		Flag("ignore-certificate-errors", true),
		UserAgent(agents.RandomUserAgent()),
		//Flag("", true),
	}
	allocationOptions = append(allocationOptions, DefaultExecAllocatorOptions...)
	allocationOptions = append(
		allocationOptions,
		WindowSize(1920, 1048),
		ExecPath("chromium"),
	)
	ep := NewExecAllocator(allocationOptions...)
	// OTHER FLAG CONFIGS:
	Incognito(ep) // TODO: use this?
	Headless(ep)
	NoFirstRun(ep)
	NoDefaultBrowserCheck(ep)
	DisableGPU(ep)
	//////////////////////////////////////////////////

	epCtx, cancel := context.WithCancel(ctx)
	browser, err := ep.Allocate(epCtx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	Sfln("listening on %s", browser.URL)
	Ln("browser running on:", browser.URL.Hostname()+":"+browser.URL.Port())

	// wait some seconds for the browser to start:
	time.Sleep(time.Second * 5)

	return browser, cancel, nil
}

// setupExecAllocator is similar to NewExecAllocator, but it allows NewContext
// to create the allocator without the unnecessary context layer.
func setupExecAllocator(opts ...ExecAllocatorOption) *ExecAllocator {
	ep := &ExecAllocator{
		initFlags: make(map[string]interface{}),
	}
	for _, o := range opts {
		o(ep)
	}
	if ep.execPath == "" {
		ep.execPath = findExecPath()
	}
	return ep
}

// DefaultExecAllocatorOptions are the ExecAllocator options used by NewContext
// if the given parent context doesn't have an allocator set up.
var DefaultExecAllocatorOptions = []ExecAllocatorOption{
	NoFirstRun,
	NoDefaultBrowserCheck,
	Headless,

	// After Puppeteer's default behavior.
	Flag("disable-background-networking", true),
	Flag("enable-features", "NetworkService,NetworkServiceInProcess"),
	Flag("disable-background-timer-throttling", true),
	Flag("disable-backgrounding-occluded-windows", true),
	Flag("disable-breakpad", true),
	Flag("disable-client-side-phishing-detection", true),
	Flag("disable-default-apps", true),
	Flag("disable-dev-shm-usage", true),
	Flag("disable-extensions", true),
	Flag("disable-features", "site-per-process,TranslateUI,BlinkGenPropertyTrees"),
	Flag("disable-hang-monitor", true),
	Flag("disable-ipc-flooding-protection", true),
	Flag("disable-popup-blocking", true),
	Flag("disable-prompt-on-repost", true),
	Flag("disable-renderer-backgrounding", true),
	Flag("disable-sync", true),
	Flag("force-color-profile", "srgb"),
	Flag("metrics-recording-only", true),
	Flag("safebrowsing-disable-auto-update", true),
	Flag("enable-automation", true),
	Flag("password-store", "basic"),
	Flag("use-mock-keychain", true),
}

// NewExecAllocator creates a new context set up with an ExecAllocator, suitable
// for use with NewContext.
func NewExecAllocator(opts ...ExecAllocatorOption) *ExecAllocator {
	return setupExecAllocator(opts...)
}

// ExecAllocatorOption is a exec allocator option.
type ExecAllocatorOption func(*ExecAllocator)

// ExecAllocator is an Allocator which starts new browser processes on the host
// machine.
type ExecAllocator struct {
	execPath  string
	initFlags map[string]interface{}

	wg sync.WaitGroup
}

// allocTempDir is used to group all ExecAllocator temporary user data dirs in
// the same location, useful for the tests. If left empty, the system's default
// temporary directory is used.
var allocTempDir string

// Browser is the high-level Chrome DevTools Protocol browser manager, handling
// the browser process runner, WebSocket clients, associated targets, and
// network, page, and DOM events.
type Browser struct {
	// LostConnection is closed when the websocket connection to Chrome is
	// dropped. This can be useful to make sure that Browser's context is
	// cancelled (and the handler stopped) once the connection has failed.
	LostConnection chan struct{}

	// logging funcs
	logf func(string, ...interface{})
	errf func(string, ...interface{})
	dbgf func(string, ...interface{})

	RawURL string
	URL    *url.URL
	// process can be initialized by the allocators which start a process
	// when allocating a browser.
	process *os.Process

	// userDataDir can be initialized by the allocators which set up user
	// data dirs directly.
	userDataDir string
}

// NewBrowser creates a new browser. Typically, this function wouldn't be called
// directly, as the Allocator interface takes care of it.
func NewBrowser(ctx context.Context, urlstr string) (*Browser, error) {
	b := &Browser{
		LostConnection: make(chan struct{}),

		logf: func(format string, args ...interface{}) {
			Sfln(format, args...)
		},
	}

	// ensure errf is set
	if b.errf == nil {
		b.errf = func(s string, v ...interface{}) { b.logf("ERROR: "+s, v...) }
	}

	urlstr = forceIP(urlstr)

	browserURL, err := url.Parse(urlstr)
	if err != nil {
		return nil, fmt.Errorf("error while parsing browser URL: %s", err)
	}

	b.RawURL = urlstr
	b.URL = browserURL
	return b, nil
}

// forceIP forces the host component in urlstr to be an IP address.
//
// Since Chrome 66+, Chrome DevTools Protocol clients connecting to a browser
// must send the "Host:" header as either an IP address, or "localhost".
func forceIP(urlstr string) string {
	if i := strings.Index(urlstr, "://"); i != -1 {
		scheme := urlstr[:i+3]
		host, port, path := urlstr[len(scheme)+3:], "", ""
		if i := strings.Index(host, "/"); i != -1 {
			host, path = host[:i], host[i:]
		}
		if i := strings.Index(host, ":"); i != -1 {
			host, port = host[:i], host[i:]
		}
		if addr, err := net.ResolveIPAddr("ip", host); err == nil {
			urlstr = scheme + addr.IP.String() + port + path
		}
	}
	return urlstr
}

// Allocate satisfies the Allocator interface.
func (a *ExecAllocator) Allocate(ctx context.Context) (*Browser, error) {
	var args []string
	for name, value := range a.initFlags {
		switch value := value.(type) {
		case string:
			args = append(args, fmt.Sprintf("--%s=%s", name, value))
		case bool:
			if value {
				args = append(args, fmt.Sprintf("--%s", name))
			}
		default:
			return nil, fmt.Errorf("invalid exec pool flag")
		}
	}

	removeDir := false
	dataDir, ok := a.initFlags["user-data-dir"].(string)
	if !ok {
		tempDir, err := ioutil.TempDir(allocTempDir, "chromedp-runner")
		if err != nil {
			return nil, err
		}
		args = append(args, "--user-data-dir="+tempDir)
		dataDir = tempDir
		removeDir = true
	}
	if _, ok := a.initFlags["no-sandbox"]; !ok && os.Getuid() == 0 {
		// Running as root, for example in a Linux container. Chrome
		// needs --no-sandbox when running as root, so make that the
		// default, unless the user set Flag("no-sandbox", false).
		args = append(args, "--no-sandbox")
	}
	args = append(args, "--remote-debugging-port=0")

	// Force the first page to be blank, instead of the welcome page;
	// --no-first-run doesn't enforce that.
	args = append(args, "about:blank")

	cmd := exec.CommandContext(ctx, a.execPath, args...)
	defer func() {
		if removeDir && cmd.Process == nil {
			// We couldn't start the process, so we didn't get to
			// the goroutine that handles RemoveAll below. Remove it
			// to not leave an empty directory.
			os.RemoveAll(dataDir)
		}
	}()
	allocateCmdOptions(cmd)

	// We must start the cmd before calling cmd.Wait, as otherwise the two
	// can run into a data race.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	a.wg.Add(1) // for the entire allocator
	go func() {
		<-ctx.Done()
		// First wait for the process to be finished.
		// TODO: do we care about this error in any scenario? if the
		// user cancelled the context and killed chrome, this will most
		// likely just be "signal: killed", which isn't interesting.
		cmd.Wait()
		// Then delete the temporary user data directory, if needed.
		if removeDir {
			if err := os.RemoveAll(dataDir); err == nil {
			}
		}
		a.wg.Done()
	}()
	wsURL, err := addrFromStderr(stderr)
	if err != nil {
		return nil, err
	}

	browser, err := NewBrowser(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	go func() {
		// If the browser loses connection, kill the entire process and
		// handler at once.
		<-browser.LostConnection
	}()
	browser.process = cmd.Process
	browser.userDataDir = dataDir
	return browser, nil
}

func allocateCmdOptions(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	// When the parent process dies (Go), kill the child as well.
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// addrFromStderr finds the free port that Chrome selected for the debugging
// protocol. This should be hooked up to a new Chrome process's Stderr pipe
// right after it is started.
func addrFromStderr(rc io.ReadCloser) (string, error) {
	defer rc.Close()
	url := ""
	scanner := bufio.NewScanner(rc)
	prefix := "DevTools listening on"

	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if s := strings.TrimPrefix(line, prefix); s != line {
			url = strings.TrimSpace(s)
			break
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("chrome stopped too early; stderr:\n%s",
			strings.Join(lines, "\n"))
	}
	return url, nil
}

// Wait satisfies the Allocator interface.
func (a *ExecAllocator) Wait() {
	a.wg.Wait()
}

// ExecPath returns an ExecAllocatorOption which uses the given path to execute
// browser processes. The given path can be an absolute path to a binary, or
// just the name of the program to find via exec.LookPath.
func ExecPath(path string) ExecAllocatorOption {
	return func(a *ExecAllocator) {
		// Convert to an absolute path if possible, to avoid
		// repeated LookPath calls in each Allocate.
		if fullPath, _ := exec.LookPath(path); fullPath != "" {
			a.execPath = fullPath
		} else {
			a.execPath = path
		}
	}
}

// findExecPath tries to find the Chrome browser somewhere in the current
// system. It performs a rather agressive search, which is the same in all
// systems. That may make it a bit slow, but it will only be run when creating a
// new ExecAllocator.
func findExecPath() string {
	for _, path := range [...]string{
		// Unix-like
		"headless_shell",
		"headless-shell",
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"google-chrome-unstable",
		"/usr/bin/google-chrome",

		// Windows
		"chrome",
		"chrome.exe", // in case PATHEXT is misconfigured
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,

		// Mac
		`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
	} {
		found, err := exec.LookPath(path)
		if err == nil {
			return found
		}
	}
	// Fall back to something simple and sensible, to give a useful error
	// message.
	return "google-chrome"
}

// Flag is a generic command line option to pass a flag to Chrome. If the value
// is a string, it will be passed as --name=value. If it's a boolean, it will be
// passed as --name if value is true.
func Flag(name string, value interface{}) ExecAllocatorOption {
	return func(a *ExecAllocator) {
		a.initFlags[name] = value
	}
}

// UserDataDir is the command line option to set the user data dir.
//
// Note: set this option to manually set the profile directory used by Chrome.
// When this is not set, then a default path will be created in the /tmp
// directory.
func UserDataDir(dir string) ExecAllocatorOption {
	return Flag("user-data-dir", dir)
}

// ProxyServer is the command line option to set the outbound proxy server.
func ProxyServer(proxy string) ExecAllocatorOption {
	return Flag("proxy-server", proxy)
}

// WindowSize is the command line option to set the initial window size.
func WindowSize(width, height int) ExecAllocatorOption {
	return Flag("window-size", fmt.Sprintf("%d,%d", width, height))
}

// UserAgent is the command line option to set the default User-Agent
// header.
func UserAgent(userAgent string) ExecAllocatorOption {
	return Flag("user-agent", userAgent)
}

// NoSandbox is the Chrome comamnd line option to disable the sandbox.
func NoSandbox(a *ExecAllocator) {
	Flag("no-sandbox", true)(a)
}

// NoFirstRun is the Chrome comamnd line option to disable the first run
// dialog.
func NoFirstRun(a *ExecAllocator) {
	Flag("no-first-run", true)(a)
}

// NoDefaultBrowserCheck is the Chrome comamnd line option to disable the
// default browser check.
func NoDefaultBrowserCheck(a *ExecAllocator) {
	Flag("no-default-browser-check", true)(a)
}

// Headless is the command line option to run in headless mode. On top of
// setting the headless flag, it also hides scrollbars and mutes audio.
func Headless(a *ExecAllocator) {
	Flag("headless", true)(a)
	// Like in Puppeteer.
	Flag("hide-scrollbars", true)(a)
	Flag("mute-audio", true)(a)
}

// DisableGPU is the command line option to disable the GPU process.
func DisableGPU(a *ExecAllocator) {
	Flag("disable-gpu", true)(a)
}

// Incognito is the command line option for incognito mode.
func Incognito(a *ExecAllocator) {
	Flag("incognito", true)(a)
}

// IgnoreCertificateErrors is the command line option for ignoring certificate errors
func IgnoreCertificateErrors(a *ExecAllocator) {
	Flag("ignore-certificate-errors", true)(a)
}

///////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

func AbortOnErrors(ctx context.Context, c *cdp.Client, abort chan<- error) error {
	exceptionThrown, err := c.Runtime.ExceptionThrown(ctx)
	if err != nil {
		return err
	}

	loadingFailed, err := c.Network.LoadingFailed(ctx)
	if err != nil {
		return err
	}

	go func() {
		defer exceptionThrown.Close() // Cleanup.
		defer loadingFailed.Close()
		for {
			select {
			// Check for exceptions so we can abort as soon
			// as one is encountered.
			case <-exceptionThrown.Ready():
				ev, err := exceptionThrown.Recv()
				if err != nil {
					// This could be any one of: stream closed,
					// connection closed, context deadline or
					// unmarshal failed.
					abort <- err
					return
				}

				// Ruh-roh! Let the caller know something went wrong.
				abort <- ev.ExceptionDetails

			// Check for non-canceled resources that failed
			// to load.
			case <-loadingFailed.Ready():
				ev, err := loadingFailed.Recv()
				if err != nil {
					abort <- err
					return
				}

				// For now, most optional fields are pointers
				// and must be checked for nil.
				canceled := ev.Canceled != nil && *ev.Canceled

				if !canceled {
					abort <- fmt.Errorf("request %s failed: %s", ev.RequestID, ev.ErrorText)
				}
			}
		}
	}()
	return nil
}

var (
	DenyDownload = "deny"
)

func stringPtr(s string) *string {
	return &s
}

type causer interface {
	Cause() error
}

// Cause returns the underlying cause for this error, if possible.
// If err does not implement causer.Cause(), then err is returned.
func Cause(err error) error {
	for err != nil {
		if c, ok := err.(causer); ok {
			err = c.Cause()
		} else {
			return err
		}
	}
	return err
}
