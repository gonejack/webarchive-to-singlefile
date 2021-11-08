package model

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/go-resty/resty/v2"
	"github.com/vincent-petithory/dataurl"
)

const defaultContentType = "text/html; charset=utf-8"

var ErrMissingBoundary = errors.New("no boundary found for multipart entity")

type trimReader struct {
	reader  io.Reader
	trimmed bool
}

func (tr *trimReader) Read(buf []byte) (int, error) {
	n, err := tr.reader.Read(buf)
	if err != nil {
		return n, err
	}
	if !tr.trimmed {
		t := bytes.TrimLeftFunc(buf[:n], unicode.IsSpace)
		tr.trimmed = true
		n = copy(buf, t)
	}
	return n, err
}

type part struct {
	header  textproto.MIMEHeader
	body    []byte
	mime    string
	dataURI string
}

func (p *part) DataURI() string {
	return dataurl.New(p.body, p.mime).String()
}

type MHTML struct {
	parts map[string]*part

	url  string
	html *part

	client *resty.Client
}

func (m *MHTML) From(r io.Reader) (err error) {
	r = &trimReader{reader: r}
	tp := textproto.NewReader(bufio.NewReader(r))

	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return
	}

	parts, err := m.parseParts(header, tp.R)
	if err != nil {
		return
	}

	m.parts = make(map[string]*part)
	for _, p := range parts {
		loc := p.header.Get("content-location")
		if strings.HasPrefix(loc, "cid:") {
			m.parts[loc] = p
			continue
		}
		if strings.HasPrefix(loc, "data:") {
			continue
		}

		for _, r := range m.patchRef(loc) {
			_, exist := m.parts[r]
			if !exist {
				m.parts[r] = p
			}
		}

		cid := p.header.Get("content-id")
		if cid != "" {
			cid = strings.Trim(cid, "<>")
			cid = "cid:" + cid
			m.parts[cid] = p
		}
	}

	m.url = header.Get("Snapshot-Content-Location")
	m.html = m.parts[m.url]

	return
}
func (m *MHTML) HTML() []byte {
	return m.html.body
}
func (m *MHTML) MergeWarc(warc *WebArchive) {
	for _, r := range warc.Resources() {
		p := &part{
			mime: r.WebResourceMIMEType,
			body: r.WebResourceData,
		}
		for _, r := range m.patchRef(r.WebResourceURL) {
			_, exist := m.parts[r]
			if !exist {
				m.parts[r] = p
			}
		}
	}
}
func (m *MHTML) BuildEmbedHTML() (html string, err error) {
	for _, p := range m.parts {
		if p.mime == "text/css" {
			m.patchCSS(p)
		}
	}

	err = m.patchHTML(m.html)
	if err != nil {
		return
	}
	html = string(m.html.body)

	return
}
func (m *MHTML) patchCSS(p *part) {
	css := string(p.body)
	cxx := m.cssReplacer(css).Replace(css)
	p.body = []byte(cxx)
}
func (m *MHTML) patchHTML(p *part) (err error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(p.body))
	if err != nil {
		return
	}

	doc.Find("link").Each(func(i int, link *goquery.Selection) {
		rel, _ := link.Attr("rel")
		ref, _ := link.Attr("href")

		switch {
		case rel == "stylesheet":
		case rel == "apple-touch-icon":
		case rel == "shortcut icon":
		case strings.HasPrefix(ref, "data:"):
			return
		default:
			return
		}
		p, exist := m.findOrFetch(ref)
		if !exist {
			log.Printf("cannot find link %s", ref)
			return
		}
		link.SetAttr("href", p.DataURI())
	})
	doc.Find("style").Each(func(i int, e *goquery.Selection) {
		css := e.Text()
		cxx := m.cssReplacer(css).Replace(css)
		e.SetText(cxx)
	})
	doc.Find("*").Filter("[style]").Each(func(i int, e *goquery.Selection) {
		css, _ := e.Attr("style")
		cxx := m.cssReplacer(css).Replace(css)
		e.SetAttr("style", cxx)
	})
	doc.Find("img,video,source").Each(func(i int, e *goquery.Selection) {
		src, _ := e.Attr("src")
		switch {
		case src == "":
			return
		case strings.HasPrefix(src, "data:"):
			return
		}
		p, exist := m.findOrFetch(src)
		if !exist {
			log.Printf("cannot find %s source %s", e.Get(0).Data, src)
			return
		}
		e.SetAttr("src", p.DataURI())
	})
	doc.Find("iframe").Each(func(i int, iframe *goquery.Selection) {
		src, _ := iframe.Attr("src")
		if !strings.HasPrefix(src, "cid:") {
			return
		}
		p, exist := m.findOrFetch(src)
		if !exist {
			log.Printf("cannot find iframe source %s", src)
			return
		}
		loc := p.header.Get("content-location")
		if loc != "" {
			iframe.SetAttr("src", loc)
		}
	})

	html, err := doc.Html()
	if err != nil {
		return
	}

	p.body = []byte(html)

	return
}
func (m *MHTML) findOrFetch(ref string) (p *part, exist bool) {
	p, exist = m.parts[ref]
	if !exist {
		switch {
		case strings.HasPrefix(ref, "cid:"):
			log.Printf("missing %s", ref)
		default:
			_, _ = m.requestRef(m.absoluteRef(ref))
		}
	}
	p, exist = m.parts[ref]
	return
}
func (m *MHTML) cssReplacer(css string) *strings.Replacer {
	var parse = func(css string) (urls []string) {
		re := regexp.MustCompile(`url\((.+?)\)`)
		ms := re.FindAllStringSubmatch(css, -1)
		for _, m := range ms {
			u := strings.Trim(m[1], ` "'`)

			u = strings.TrimSuffix(u, "&#34;")
			u = strings.TrimPrefix(u, "&#34;")

			if !strings.HasPrefix(u, "data:") {
				urls = append(urls, u)
			}
		}
		return
	}

	var segs []string
	for _, u := range parse(css) {
		relative, absolute := m.relativeRef(u), m.absoluteRef(u)
		p, exist := m.findOrFetch(absolute)
		if exist {
			uri := p.DataURI()
			segs = append(segs, relative, uri)
			segs = append(segs, absolute, uri)
		} else {
			log.Printf("cannot find css %s", u)
		}
	}

	return strings.NewReplacer(segs...)
}
func (m *MHTML) parseParts(header textproto.MIMEHeader, reader io.Reader) ([]*part, error) {
	var ps []*part

	contentType := header.Get("content-type")
	if contentType == "" {
		contentType = defaultContentType
		header.Set("content-type", contentType)
	}
	mimeType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return ps, err
	}

	switch {
	case strings.HasPrefix(mimeType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return ps, ErrMissingBoundary
		}
		mr := multipart.NewReader(reader, boundary)
		for {
			p, err := mr.NextPart()
			if err != nil {
				if err == io.EOF {
					break
				} else {
					return ps, err
				}
			}

			contentType := p.Header.Get("content-type")
			if contentType == "" {
				contentType = defaultContentType
				p.Header.Set("Content-Type", contentType)
			}
			mimeType, _, err := mime.ParseMediaType(contentType)
			if err != nil {
				return ps, err
			}

			switch {
			case strings.HasPrefix(mimeType, "multipart/"):
				sps, err := m.parseParts(p.Header, p)
				if err != nil {
					return ps, err
				}
				ps = append(ps, sps...)
			default:
				var r io.Reader = p
				switch p.Header.Get("Content-Transfer-Encoding") {
				case "base64":
					r = base64.NewDecoder(base64.StdEncoding, r)
				}
				var b bytes.Buffer
				if _, err := io.Copy(&b, r); err != nil {
					return ps, err
				}
				ps = append(ps, &part{body: b.Bytes(), header: p.Header, mime: mimeType})
			}
		}
	default:
		encoding := header.Get("Content-Transfer-Encoding")
		switch encoding {
		case "quoted-printable":
			reader = quotedprintable.NewReader(reader)
		case "base64":
			reader = base64.NewDecoder(base64.StdEncoding, reader)
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, reader); err != nil {
			return ps, err
		}
		ps = append(ps, &part{body: b.Bytes(), header: header, mime: mimeType})
	}
	return ps, nil
}
func (m *MHTML) patchRef(ref string) (refs []string) {
	return []string{m.relativeRef(ref), m.absoluteRef(ref)}
}
func (m *MHTML) relativeRef(ref string) string {
	if strings.HasPrefix(ref, "data:") {
		return ref
	}
	u, e := url.Parse(ref)
	if e == nil {
		u.Host = ""
		u.Scheme = ""
		return u.String()
	}
	return ref
}
func (m *MHTML) absoluteRef(ref string) string {
	if strings.HasPrefix(ref, "data:") {
		return ref
	}
	u, e := url.Parse(ref)
	if e == nil {
		if u.Host == "" || u.Scheme == "" {
			w, _ := url.Parse(m.url)
			if u.Scheme == "" {
				u.Scheme = w.Scheme
			}
			if u.Host == "" {
				u.Host = w.Host
			}
			return u.String()
		}
	}
	return ref
}
func (m *MHTML) requestRef(ref string) (p *part, err error) {
	if m.client == nil {
		m.client = resty.New()
		m.client.SetTimeout(time.Minute)
		m.client.SetHeader("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/95.0.4638.69 Safari/537.36")
	}

	resp, err := m.client.R().Get(m.absoluteRef(ref))
	if err != nil {
		return
	}

	mimeType, _, _ := mime.ParseMediaType(resp.Header().Get("content-type"))
	p = &part{
		header: textproto.MIMEHeader(resp.Header()),
		mime:   mimeType,
		body:   resp.Body(),
	}
	for _, r := range m.patchRef(ref) {
		_, exist := m.parts[r]
		if !exist {
			m.parts[r] = p
		}
	}

	return
}

func NewMHTML() *MHTML {
	return new(MHTML)
}
