package model

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/vincent-petithory/dataurl"
	"howett.net/plist"
)

type WebArchive struct {
	WebMainResources    *Resource     `plist:"WebMainResource"`
	WebSubResources     []*Resource   `plist:"WebSubresources"`
	WebSubframeArchives []*WebArchive `plist:"WebSubframeArchives"`

	doc *goquery.Document
	res map[string]*Resource

	sync.Mutex
}

func (w *WebArchive) From(warc string) (err error) {
	fd, err := os.Open(warc)
	if err == nil {
		defer fd.Close()
		err = plist.NewDecoder(fd).Decode(w)
	}
	return
}
func (w *WebArchive) Doc(decorate bool) (*goquery.Document, error) {
	if w.doc == nil {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(w.WebMainResources.WebResourceData))
		if err != nil {
			return nil, err
		}
		if decorate {
			w.decorate(doc)
		}
		w.doc = doc
	}
	return w.doc, nil
}
func (w *WebArchive) PatchRef(ref string) string {
	mu, err := url.Parse(w.WebMainResources.WebResourceURL)
	if err != nil {
		return ref
	}
	ru, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if ru.Host == "" {
		ru.Host = mu.Host
	}
	if ru.Scheme == "" {
		ru.Scheme = mu.Scheme
	}
	return ru.String()
}
func (w *WebArchive) SetResource(ref string, resource *Resource) {
	w.Lock()
	defer w.Unlock()

	if w.res == nil {
		w.res = make(map[string]*Resource)
	}
	w.res[ref] = resource

	return
}
func (w *WebArchive) GetResource(ref string) (res *Resource, exist bool) {
	w.Lock()
	defer w.Unlock()

	if w.res == nil {
		w.res = make(map[string]*Resource)
		for _, res := range w.WebSubResources {
			w.res[res.WebResourceURL] = res
		}
		w.res[w.WebMainResources.WebResourceURL] = w.WebMainResources
	}
	res, exist = w.res[ref]

	return
}

func (w *WebArchive) decorate(doc *goquery.Document) {
	u, err := url.Parse(w.WebMainResources.WebResourceURL)
	if err == nil {
		switch u.Host {
		case "telegra.ph":
			doc.Find("div#_tl_link_tooltip").Remove()
			doc.Find("div#_tl_tooltip").Remove()
			doc.Find("div#_tl_blocks").Remove()
			doc.Find("header").Remove()
			doc.Find("aside").Remove()
			doc.Find("article h1").First().Remove()
		}
	}
	meta := fmt.Sprintf(`<meta name="inostar:publish" content="%s">`, w.pubTime().Format(time.RFC1123Z))
	doc.Find("head").AppendHtml(meta)
	doc.Find("body").PrependHtml(w.header()).AppendHtml(w.footer())
}
func (w *WebArchive) header() string {
	const tpl = `
<p>
	<a title="Published: {published}" href="{link}" style="display:block; color: #000; padding-bottom: 10px; text-decoration: none; font-size:1em; font-weight: normal;">
		<span style="display: block; color: #666; font-size:1.0em; font-weight: normal;">{origin}</span>
		<span style="font-size: 1.5em;">{title}</span>
	</a>
</p>`

	link := w.WebMainResources.WebResourceURL
	origin := func() string {
		content, exist := w.doc.Find(`meta[property="og:site_name"]`).Attr("content")
		if exist {
			return content
		}
		u, err := url.Parse(link)
		if err == nil {
			return u.Host
		}
		return "origin"
	}()

	replacer := strings.NewReplacer(
		"{link}", w.WebMainResources.WebResourceURL,
		"{origin}", html.EscapeString(origin),
		"{published}", w.pubTime().Format("2006-01-02 15:04:05"),
		"{title}", html.EscapeString(w.doc.Find("title").Text()),
	)

	return replacer.Replace(tpl)
}
func (w *WebArchive) footer() string {
	const tpl = `
<br/><br/>
<a style="display: inline-block; border-top: 1px solid #ccc; padding-top: 5px; color: #666; text-decoration: none;"
   href="{link}">{link}</a>
<p style="color:#999;">Save with <a style="color:#666; text-decoration:none; font-weight: bold;"
                                    href="https://github.com/gonejack/webarchive-to-html">webarchive-to-html</a>
</p>`

	return strings.NewReplacer(
		"{link}", fmt.Sprintf(w.WebMainResources.WebResourceURL),
	).Replace(tpl)
}
func (w *WebArchive) pubTime() time.Time {
	content, exist := w.doc.Find(`meta[property="article:published_time"]`).Attr("content")
	if exist {
		t, err := time.Parse("2006-01-02T15:04:05Z0700", content)
		if err == nil {
			return t
		}
	}
	return time.Now()
}

type Resource struct {
	WebResourceMIMEType         string `plist:"WebResourceMIMEType"`
	WebResourceTextEncodingName string `plist:"WebResourceTextEncodingName"`
	WebResourceURL              string `plist:"WebResourceURL"`
	WebResourceFrameName        string `plist:"WebResourceFrameName"`
	WebResourceData             []byte `plist:"WebResourceData"`
	//WebResourceResponse         interface{} `plist:"WebResourceResponse"`

	dataURI string
}

func (r *Resource) ResetData(data []byte) {
	r.WebResourceData = data
	r.dataURI = ""
}

func (r *Resource) DataURI() string {
	if r.dataURI == "" {
		r.dataURI = dataurl.New(r.WebResourceData, r.WebResourceMIMEType).String()
	}
	return r.dataURI
}
