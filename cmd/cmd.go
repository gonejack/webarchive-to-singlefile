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
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/alecthomas/kong"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
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
		fmt.Println("Visit https://github.com/gonejack/embede-html")
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
func (c *WarcToHtml) process(warcf string) (err error) {
	w := new(model.WebArchive)
	err = w.From(warcf)
	if err != nil {
		return fmt.Errorf("cannot parse %s: %s", warcf, err)
	}

	s := c.newServer(w)
	defer s.Close()

	ctx, cancel := c.newContext(s)
	defer cancel()

	html := ""
	err = chromedp.Run(ctx,
		chromedp.Navigate(w.WebMainResources.WebResourceURL),
		chromedp.Sleep(time.Second*5),
		chromedp.ActionFunc(func(ctx context.Context) error {
			scroll := `$('html, body').animate({scrollTop:$(document).height()}, 4000, 'linear');`
			_, exp, err := runtime.Evaluate(scroll).Do(ctx)
			if err != nil {
				return err
			}
			if exp != nil {
				return exp
			}
			return nil
		}),
		chromedp.Sleep(time.Second*5),
		chromedp.OuterHTML("html", &html),
		//chromedp.ActionFunc(func(ctx context.Context) error {
		//	dat, _, err := page.PrintToPDF().WithPrintBackground(true).Do(ctx)
		//	if err != nil {
		//		return err
		//	}
		//	return os.WriteFile("output.pdf", dat, 0766)
		//}),
	)
	if err != nil {
		return fmt.Errorf("cannot render %s: %s", warcf, err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return fmt.Errorf("cannot parse html %s: %s", html, err)
	}

	c.patchCSS(doc, w)
	c.patchHTML(doc, w)

	html, err = doc.Html()
	if err != nil {
		return fmt.Errorf("cannot generate html: %s", err)
	}

	output := strings.TrimSuffix(warcf, ".webarchive") + ".html"
	return os.WriteFile(output, []byte(html), 0766)
}
func (c *WarcToHtml) newContext(server *httptest.Server) (context.Context, context.CancelFunc) {
	opts := []chromedp.ExecAllocatorOption{
		//chromedp.Headless,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,

		// After Puppeteer's default behavior.
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("enable-features", "NetworkService,NetworkServiceInProcess"),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-features", "site-per-process,Translate,BlinkGenPropertyTrees"),
		chromedp.Flag("disable-hang-monitor", true),
		chromedp.Flag("disable-ipc-flooding-protection", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-prompt-on-repost", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("safebrowsing-disable-auto-update", true),
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.ProxyServer(server.URL),
	}
	ctx, _ := chromedp.NewExecAllocator(context.TODO(), opts...)
	ctx, cancel := chromedp.NewContext(ctx, chromedp.WithBrowserOption(
		chromedp.WithDialTimeout(time.Minute),
	))
	return ctx, cancel
}
func (c *WarcToHtml) newServer(warc *model.WebArchive) *httptest.Server {
	p := c.newProxy()
	p.OnRequest().DoFunc(func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		r.URL.Host = r.Host
		url := r.URL.String()

		res, exist := warc.GetResource(url)
		if exist {
			if c.Verbose {
				log.Printf("read local: %s", url)
			}

			rsp := &http.Response{
				Status:           http.StatusText(200),
				StatusCode:       200,
				Request:          r,
				TransferEncoding: r.TransferEncoding,
				ContentLength:    int64(len(res.WebResourceData)),
				Body:             io.NopCloser(bytes.NewReader(res.WebResourceData)),
			}
			rsp.Header = make(http.Header)
			rsp.Header.Set("Content-Type", res.WebResourceMIMEType)

			return r, rsp
		} else {
			if c.Verbose {
				log.Printf("read remote: %s", url)
			}
			return r, nil
		}
	})
	p.OnResponse().DoFunc(func(r *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if r.Request == nil || r.Request.Body == nil {
			return r
		}

		url := r.Request.URL.String()
		_, exist := warc.GetResource(url)
		if !exist {
			rec := model.NewBodyRecorder(r.Body)
			r.Body = rec
			go func() {
				body := rec.Body()
				res := &model.Resource{
					WebResourceMIMEType:         r.Header.Get("content-type"),
					WebResourceTextEncodingName: r.Header.Get("content-encoding"),
					WebResourceURL:              url,
					WebResourceData:             body,
				}
				if c.Verbose {
					log.Printf("caching: %s", url)
				}
				warc.SetResource(url, res)
			}()
		}

		return r
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
func (c *WarcToHtml) parseCSS(css string) (urls []string) {
	re := regexp.MustCompile(`url\((.+?)\)`)
	ms := re.FindAllStringSubmatch(css, -1)
	for _, m := range ms {
		u := strings.Trim(m[1], ` "'`)
		if strings.HasPrefix(u, "data:") {
			continue
		}
		urls = append(urls, u)
	}
	return
}
func (c *WarcToHtml) patchCSS(doc *goquery.Document, warc *model.WebArchive) {
	if c.Verbose {
		log.Printf("patch CSS references")
	}

	var replacer = func(css string) *strings.Replacer {
		var ps []string
		for _, ref := range c.parseCSS(css) {
			r, exist := warc.GetResource(ref)
			if !exist {
				r, exist = warc.GetResource(warc.PatchRef(ref))
			}
			if exist {
				if c.Verbose {
					log.Printf("patching %s", ref)
				}
				ps = append(ps, ref, r.DataURI())
			}
		}
		return strings.NewReplacer(ps...)
	}
	for _, r := range warc.Resources() {
		if r.WebResourceMIMEType == "text/css" {
			css := string(r.WebResourceData)
			cxx := replacer(css).Replace(css)
			r.ResetData([]byte(cxx))
		}
	}
	doc.Find("style").Each(func(i int, style *goquery.Selection) {
		css := style.Text()
		cxx := replacer(css).Replace(css)
		style.SetText(cxx)
	})
}
func (c *WarcToHtml) patchHTML(doc *goquery.Document, warc *model.WebArchive) {
	if c.Verbose {
		log.Printf("patch HTML references")
	}

	doc.Find("img,script,link").Each(func(i int, e *goquery.Selection) {
		attr := "src"
		switch e.Get(0).Data {
		case "img":
			e.RemoveAttr("srcset")
		case "link":
			rel, _ := e.Attr("rel")
			switch rel {
			default:
				return
			case "shortcut icon":
			case "stylesheet":
			case "icon":
			case "shortcut":
			}
			attr = "href"
		}
		ref, _ := e.Attr(attr)
		switch {
		case ref == "":
			return
		case strings.HasPrefix(ref, "data:"):
			return
		default:
			if c.Verbose {
				log.Printf("patching %s", ref)
			}
			r, exist := warc.GetResource(ref)
			if !exist {
				r, exist = warc.GetResource(warc.PatchRef(ref))
			}
			if exist {
				e.SetAttr(attr, r.DataURI())
			} else {
				log.Printf("cannot find repalcement for %s", ref)
			}
		}
	})
}
