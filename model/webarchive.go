package model

import (
	"net/url"
	"os"
	"sync"

	"github.com/PuerkitoBio/goquery"
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
func (w *WebArchive) Resources() map[string]*Resource {
	return w.res
}

type Resource struct {
	WebResourceMIMEType         string `plist:"WebResourceMIMEType"`
	WebResourceTextEncodingName string `plist:"WebResourceTextEncodingName"`
	WebResourceURL              string `plist:"WebResourceURL"`
	WebResourceFrameName        string `plist:"WebResourceFrameName"`
	WebResourceData             []byte `plist:"WebResourceData"`
	//WebResourceResponse         interface{} `plist:"WebResourceResponse"`
}
