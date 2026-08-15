package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/elazarl/goproxy"

	"github.com/gonejack/webarchive-to-singlefile/javascript"
	"github.com/gonejack/webarchive-to-singlefile/model"
)

type options struct {
	Verbose        bool          `short:"v" help:"Verbose printing."`
	DisableJS      bool          `short:"j" help:"Disable JavaScript."`
	ChromePath     string        `name:"chrome-path" placeholder:"PATH" help:"Chrome or Chromium executable path. Defaults to CHROME_PATH or auto-detection."`
	Visible        bool          `name:"visible" help:"Show the browser window while rendering."`
	MaxLoadingTime time.Duration `name:"max-loading-time" default:"10s" help:"Maximum time to wait for page loading."`

	About      bool     `help:"About."`
	WebArchive []string `arg:"" optional:""`
}

type WarcToHtml struct {
	options
}

func (c *WarcToHtml) Run() (err error) {
	kong.Parse(&c.options,
		kong.Name("webarchive-to-singlefile"),
		kong.Description("This command line converts Safari's .webarchive file to complete .html."),
		kong.UsageOnError(),
	)
	if c.About {
		fmt.Println("Visit https://github.com/gonejack/webarchive-to-singlefile")
		return
	}
	if len(c.WebArchive) == 0 || c.WebArchive[0] == "*.webarchive" {
		c.WebArchive, _ = filepath.Glob("*.webarchive")
	}
	if len(c.WebArchive) == 0 {
		return errors.New("no .webarchive file given")
	}

	return c.run()
}
func (c *WarcToHtml) run() (err error) {
	for _, warc := range c.WebArchive {
		log.Printf("process %s", warc)
		if e := c.process(warc); e != nil {
			return e
		}
	}
	return
}
func (c *WarcToHtml) process(warc string) (err error) {
	w := new(model.WebArchive)
	err = w.From(warc)
	if err != nil {
		return fmt.Errorf("cannot parse %s: %w", warc, err)
	}

	server := c.newServer(w)
	defer server.Close()

	ctx, cancel, err := c.newContext(server)
	if err != nil {
		return fmt.Errorf("cannot render %s: %w", warc, err)
	}
	defer cancel()

	err = chromedp.Run(ctx)
	if err != nil {
		return fmt.Errorf("cannot start browser for %s: %w", warc, err)
	}
	// loading
	{
		cxx, cancel := context.WithTimeout(ctx, c.MaxLoadingTime)
		defer cancel()
		err = chromedp.Run(cxx, chromedp.Navigate(w.WebMainResources.WebResourceURL))
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("cannot navigate %s: %w", warc, err)
			}
			log.Printf("navigation timed out after %s, continue rendering", c.MaxLoadingTime)
			if err = chromedp.Run(ctx, page.StopLoading()); err != nil {
				return fmt.Errorf("cannot stop loading %s: %w", warc, err)
			}
		}
	}
	// rendering
	var snapshot string
	{
		err = chromedp.Run(ctx,
			chromedp.Evaluate(javascript.Scroll, nil, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
				return params.WithAwaitPromise(true)
			}),
			chromedp.Sleep(time.Second*1),
			chromedp.ActionFunc(func(ctx context.Context) (err error) {
				snapshot, err = page.CaptureSnapshot().Do(ctx)
				return nil
			}),
		)
		if err != nil {
			return fmt.Errorf("cannot render %s: %w", warc, err)
		}
	}

	mhtml := model.NewMHTML()
	err = mhtml.From(strings.NewReader(snapshot))
	if err != nil {
		return fmt.Errorf("cannot parse mhtml: %w", err)
	}
	mhtml.MergeWarc(w)

	htm, err := mhtml.BuildEmbedHTML()
	if err != nil {
		return fmt.Errorf("cannot build embed html: %w", err)
	}

	htmlfile := strings.TrimSuffix(warc, ".webarchive") + ".html"
	return os.WriteFile(htmlfile, []byte(htm), 0766)
}
func (c *WarcToHtml) newContext(server *httptest.Server) (context.Context, context.CancelFunc, error) {
	opts := slices.Concat(
		chromedp.DefaultExecAllocatorOptions[:],
		[]chromedp.ExecAllocatorOption{
			chromedp.IgnoreCertErrors,
			chromedp.ProxyServer(server.URL),
			chromedp.Flag("disable-features", "Translate,TranslateUI"),
		},
	)
	where, err := c.findBrowser()
	if err != nil {
		return nil, nil, err
	}
	if where != "" {
		opts = append(opts, chromedp.ExecPath(where))
	}
	if c.Visible {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if c.DisableJS {
		opts = append(opts, chromedp.Flag("blink-settings", "scriptEnabled=false"))
	}
	ctx, _ := chromedp.NewExecAllocator(context.TODO(), opts...)
	ctx, cancel := chromedp.NewContext(ctx, chromedp.WithBrowserOption(
		chromedp.WithDialTimeout(time.Minute),
	))
	return ctx, cancel, nil
}
func (c *WarcToHtml) newServer(warc *model.WebArchive) *httptest.Server {
	p := c.newProxy()
	p.OnRequest().DoFunc(func(rq *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if rq.Header.Get("upgrade") == "websocket" {
			return rq, nil
		}
		rq.URL.Host = rq.Host
		url := rq.URL.String()
		res, exist := warc.GetResource(url)
		if exist {
			if c.Verbose {
				log.Printf("local: %s", url)
			}
			rp := &http.Response{
				Status:           http.StatusText(200),
				StatusCode:       200,
				Request:          rq,
				TransferEncoding: rq.TransferEncoding,
				ContentLength:    int64(len(res.WebResourceData)),
				Body:             io.NopCloser(bytes.NewReader(res.WebResourceData)),
			}
			rp.Header = make(http.Header)
			rp.Header.Set("Content-Type", res.WebResourceMIMEType)
			return rq, rp
		}
		if c.Verbose {
			log.Printf("remote: %s", url)
		}
		return rq, nil
	})
	p.OnResponse().DoFunc(func(rp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if rp == nil || rp.Request == nil || rp.Request.Body == nil {
			return rp
		}
		url := rp.Request.URL.String()
		_, exist := warc.GetResource(url)
		if !exist {
			var body bytes.Buffer
			_, _ = io.Copy(&body, rp.Body)
			_ = rp.Body.Close()
			rp.Body = io.NopCloser(&body)
			res := &model.Resource{
				WebResourceMIMEType:         rp.Header.Get("content-type"),
				WebResourceTextEncodingName: rp.Header.Get("content-encoding"),
				WebResourceURL:              url,
				WebResourceData:             body.Bytes(),
			}
			if c.Verbose {
				log.Printf("cached: %s", url)
			}
			warc.SetResource(url, res)
		}
		return rp
	})
	return httptest.NewServer(p)
}
func (c *WarcToHtml) newProxy() *goproxy.ProxyHttpServer {
	p := goproxy.NewProxyHttpServer()
	//p.Verbose = true
	p.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			_, _ = w.Write([]byte("Cannot handle requests without Host header, e.g., HTTP 1.0"))
		} else {
			r.URL.Scheme = "http"
			r.URL.Host = r.Host
			p.ServeHTTP(w, r)
		}
	})
	p.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	return p
}
func (c *WarcToHtml) findBrowser() (string, error) {
	preset := cmp.Or(c.ChromePath, os.Getenv("CHROME_PATH"))
	if preset != "" {
		path, err := exec.LookPath(preset)
		if err != nil {
			return "", fmt.Errorf("cannot find Chrome executable %q: %w", preset, err)
		}
		return path, nil
	}
	switch goruntime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		if home == "" {
			return "", nil
		}
		applications := filepath.Join(home, "Applications")
		candidates := []string{
			filepath.Join(applications, "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
			filepath.Join(applications, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
			filepath.Join(applications, "Chromium.app", "Contents", "MacOS", "Chromium"),
			filepath.Join(applications, "Brave Browser.app", "Contents", "MacOS", "Brave Browser"),
			filepath.Join(applications, "Microsoft Edge.app", "Contents", "MacOS", "Microsoft Edge"),
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return "", nil
}
