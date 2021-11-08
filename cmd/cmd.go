package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/elazarl/goproxy"

	"github.com/gonejack/webarchive-to-singlefile/model"
)

type options struct {
	Verbose    bool     `short:"v" help:"Verbose printing."`
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
		return fmt.Errorf("cannot parse %s: %s", warc, err)
	}

	s := c.newServer(w)
	defer s.Close()

	ctx, cancel := c.newContext(s)
	defer cancel()

	var snapshot string
	err = chromedp.Run(ctx,
		chromedp.Navigate(w.WebMainResources.WebResourceURL),
		chromedp.Sleep(time.Second*3),
		chromedp.ActionFunc(func(ctx context.Context) error {
			scroll := `$('html, body').animate({scrollTop:$(document).height()}, 4000, 'linear');`
			_, expt, _ := runtime.Evaluate(scroll).Do(ctx)
			if expt != nil {
				_ = chromedp.KeyEvent(kb.End).Do(ctx)
				_ = chromedp.KeyEvent(kb.Home).Do(ctx)
			}
			return nil
		}),
		chromedp.Sleep(time.Second*3),
		chromedp.ActionFunc(func(ctx context.Context) (err error) {
			snapshot, err = page.CaptureSnapshot().Do(ctx)
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("cannot render %s: %s", warc, err)
	}

	mhtml := model.NewMHTML()
	err = mhtml.From(strings.NewReader(snapshot))
	if err != nil {
		return fmt.Errorf("cannot parse mhtml: %s", err)
	}
	mhtml.MergeWarc(w)

	htm, err := mhtml.BuildEmbedHTML()
	if err != nil {
		return fmt.Errorf("cannot build embed html: %s", err)
	}

	htmlfile := strings.TrimSuffix(warc, ".webarchive") + ".html"
	return os.WriteFile(htmlfile, []byte(htm), 0766)
}
func (c *WarcToHtml) newContext(server *httptest.Server) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.IgnoreCertErrors,
		chromedp.ProxyServer(server.URL),
	)
	ctx, _ := chromedp.NewExecAllocator(context.TODO(), opts...)
	ctx, cancel := chromedp.NewContext(ctx, chromedp.WithBrowserOption(
		chromedp.WithDialTimeout(time.Minute),
	))
	return ctx, cancel
}
func (c *WarcToHtml) newServer(warc *model.WebArchive) *httptest.Server {
	p := c.newProxy()
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Header.Get("upgrade") == "websocket" {
			return req, nil
		}

		req.URL.Host = req.Host
		url := req.URL.String()

		res, exist := warc.GetResource(url)
		if exist {
			if c.Verbose {
				log.Printf("local: %s", url)
			}

			rsp := &http.Response{
				Status:           http.StatusText(200),
				StatusCode:       200,
				Request:          req,
				TransferEncoding: req.TransferEncoding,
				ContentLength:    int64(len(res.WebResourceData)),
				Body:             io.NopCloser(bytes.NewReader(res.WebResourceData)),
			}
			rsp.Header = make(http.Header)
			rsp.Header.Set("Content-Type", res.WebResourceMIMEType)

			return req, rsp
		} else {
			if c.Verbose {
				log.Printf("remote: %s", url)
			}
			return req, nil
		}
	})
	p.OnResponse().DoFunc(func(rsp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if rsp.Request == nil || rsp.Request.Body == nil {
			return rsp
		}

		url := rsp.Request.URL.String()
		_, exist := warc.GetResource(url)
		if !exist {
			var b bytes.Buffer
			_, _ = io.Copy(&b, rsp.Body)
			_ = rsp.Body.Close()
			rsp.Body = io.NopCloser(&b)
			res := &model.Resource{
				WebResourceMIMEType:         rsp.Header.Get("content-type"),
				WebResourceTextEncodingName: rsp.Header.Get("content-encoding"),
				WebResourceURL:              url,
				WebResourceData:             b.Bytes(),
			}
			if c.Verbose {
				log.Printf("cached: %s", url)
			}
			warc.SetResource(url, res)
		}

		return rsp
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
