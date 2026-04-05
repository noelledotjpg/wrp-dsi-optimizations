// WRP ISMAP / ChromeDP routines
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
	"github.com/tenox7/gip"
)

type cachedImg struct {
	buf bytes.Buffer
}

type cachedMap struct {
	req wrpReq
}

// cachedSlice holds one horizontal strip of a sliced page screenshot.
// yOffset is the pixel distance from the top of the full screenshot to the
// top of this strip.  It is added back to the ISMAP y-coordinate on click.
type cachedSlice struct {
	buf     bytes.Buffer
	yOffset int
}

type wrpCache struct {
	sync.Mutex
	imgs    map[string]cachedImg
	maps    map[string]cachedMap
	slices  map[string]cachedSlice
	sliceWg sync.WaitGroup // tracks in-flight /slice/ requests
}

func (c *wrpCache) addImg(path string, buf bytes.Buffer) {
	c.Lock()
	defer c.Unlock()
	c.imgs[path] = cachedImg{buf: buf}
}

func (c *wrpCache) getImg(path string) (bytes.Buffer, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.imgs[path]
	if !ok {
		return bytes.Buffer{}, false
	}
	return e.buf, true
}

func (c *wrpCache) addMap(path string, req wrpReq) {
	c.Lock()
	defer c.Unlock()
	c.maps[path] = cachedMap{req: req}
}

func (c *wrpCache) getMap(path string) (wrpReq, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.maps[path]
	if !ok {
		return wrpReq{}, false
	}
	return e.req, true
}

func (c *wrpCache) addSlice(path string, sl cachedSlice) {
	c.Lock()
	defer c.Unlock()
	c.slices[path] = sl
}

func (c *wrpCache) getSlice(path string) (cachedSlice, bool) {
	c.Lock()
	defer c.Unlock()
	sl, ok := c.slices[path]
	return sl, ok
}

func (c *wrpCache) clear() {
	// Wait for any in-flight /slice/ requests to finish before wiping
	// the cache, otherwise the DSi gets 404s mid-page-load.
	c.sliceWg.Wait()
	c.Lock()
	defer c.Unlock()
	c.imgs = make(map[string]cachedImg)
	c.maps = make(map[string]cachedMap)
	c.slices = make(map[string]cachedSlice)
}

func chromedpStart() (context.CancelFunc, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if *userAgent == "jnrbsn" {
		if ua := fetchJnrbsnUserAgent(); ua != "" {
			*userAgent = ua
		}
	}
	if *userAgent != "" {
		opts = append(opts, chromedp.UserAgent(*userAgent))
	}
	if *browserPath != "" {
		opts = append(opts, chromedp.ExecPath(*browserPath))
	}
	if *userDataDir != "" {
		opts = append(opts, chromedp.UserDataDir(*userDataDir))
	}
	actx, acncl = chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cncl = chromedp.NewContext(actx)
	return cncl, acncl
}

func (rq *wrpReq) action() chromedp.Action {
	if rq.mouseX > 0 && rq.mouseY > 0 {
		log.Printf("%s Mouse Click %d,%d\n", rq.r.RemoteAddr, rq.mouseX, rq.mouseY)
		return chromedp.MouseClickXY(float64(rq.mouseX)/float64(rq.zoom), float64(rq.mouseY)/float64(rq.zoom))
	}
	if len(rq.buttons) > 0 {
		log.Printf("%s Button %v\n", rq.r.RemoteAddr, rq.buttons)
		switch rq.buttons {
		case "Bk":
			return chromedp.NavigateBack()
		case "St":
			return chromedp.Stop()
		case "Re":
			return chromedp.Reload()
		case "Bs":
			return chromedp.KeyEvent("\b")
		case "Rt":
			return chromedp.KeyEvent("\r")
		case "<":
			return chromedp.KeyEvent("\u0302")
		case "^":
			return chromedp.KeyEvent("\u0304")
		case "v":
			return chromedp.KeyEvent("\u0301")
		case ">":
			return chromedp.KeyEvent("\u0303")
		case "Up":
			return chromedp.KeyEvent("\u0308")
		case "Dn":
			return chromedp.KeyEvent("\u0307")
		case "All":
			return chromedp.KeyEvent("a", chromedp.KeyModifiers(input.ModifierCtrl))
		}
	}
	if len(rq.keys) > 0 {
		log.Printf("%s Sending Keys: %#v\n", rq.r.RemoteAddr, rq.keys)
		return chromedp.KeyEvent(rq.keys)
	}
	log.Printf("%s Processing Navigate Request for %s\n", rq.r.RemoteAddr, rq.url)
	return chromedp.Navigate(rq.url)
}

func (rq *wrpReq) navigate() {
	ctxErr(chromedp.Run(ctx, rq.action()), rq.w)
}

func ctxErr(err error, w io.Writer) {
	if err == nil {
		return
	}
	log.Printf("Context error: %s", err)
	fmt.Fprintf(w, "Context error: %s<BR>\n", err)
	if err.Error() != "context canceled" {
		return
	}
	ctx, cncl = chromedp.NewContext(actx)
	log.Printf("Created new context, try again")
	fmt.Fprintln(w, "Created new context, try again")
}

func waitForRender() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		timeout := *delay
		ch := make(chan struct{}, 1)
		lctx, lcancel := context.WithCancel(ctx)
		defer lcancel()
		chromedp.ListenTarget(lctx, func(ev interface{}) {
			if e, ok := ev.(*page.EventLifecycleEvent); ok && e.Name == "networkAlmostIdle" {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		})
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case <-ch:
				return nil
			default:
			}
			var ready bool
			if err := chromedp.Evaluate(
				`document.readyState === "complete" && Array.from(document.images).every(i => i.complete)`,
				&ready,
			).Do(ctx); err != nil {
				return nil
			}
			if ready {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
}

func chromedpCaptureScreenshot(res *[]byte, h int64) chromedp.Action {
	if res == nil {
		panic("res cannot be nil")
	}
	if h == 0 {
		return chromedp.CaptureScreenshot(res)
	}
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		*res, err = page.CaptureScreenshot().Do(ctx)
		return err
	})
}

// encodeImage encodes a Go image.Image into the requested WRP image format.
func (rq *wrpReq) encodeImage(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	switch rq.imgType {
	case "gip":
		if err := gip.Encode(&buf, img, nil); err != nil {
			return nil, err
		}
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
	case "gif":
		if err := gif.Encode(&buf, gifPalette(img, rq.nColors), &gif.Options{}); err != nil {
			return nil, err
		}
	default: // jpg
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: int(rq.jQual)}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func (rq *wrpReq) imgExt() string {
	if rq.imgType == "gip" {
		return "gif"
	}
	return rq.imgType
}

// ── Slice entry ───────────────────────────────────────────────────────────────

// sliceEntry is the per-strip data passed to the HTML template.
type sliceEntry struct {
	ImgURL string // served from /slice/
	MapURL string // served from /map/
	Width  int
	Height int
}

// ── captureScreenshot ─────────────────────────────────────────────────────────

func (rq *wrpReq) captureScreenshot() {
	wrpCach.clear()
	var h int64
	var pngCap []byte

	// ── Single CDP roundtrip: set narrow height, get layout metrics,
	// resize to full height, wait for render, then screenshot. ────────────
	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), 10, rq.zoom, false),
		chromedp.Location(&rq.url),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, _, _, s, err := page.GetLayoutMetrics().Do(ctx)
			if err == nil {
				h = int64(math.Ceil(s.Height))
			}
			return nil
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			height := int64(float64(rq.height) / rq.zoom)
			if rq.height == 0 && h > 0 {
				height = h + 30
			}
			return emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), height, rq.zoom, false).Do(ctx)
		}),
		waitForRender(),
	)
	if rq.proxy {
		rq.url = strings.Replace(rq.url, "https://", "http://", 1)
	}
	log.Printf("%s Landed on: %s, Height: %v\n", rq.r.RemoteAddr, rq.url, h)

	ctxErr(chromedp.Run(ctx, chromedpCaptureScreenshot(&pngCap, rq.height)), rq.w)

	// Decode the full-page PNG once; all paths below use this image.
	fullImg, err := png.Decode(bytes.NewReader(pngCap))
	if err != nil {
		log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
		fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
		return
	}

	seq := shortuuid.New()
	ext := rq.imgExt()

	// ── Slice mode (DSi-only, not proxy) ─────────────────────────────────
	if *sliceHeight > 0 && !rq.proxy {
		entries := rq.sliceAndCache(fullImg, seq, ext)
		if len(entries) > 1 {
			sSize := fmt.Sprintf("%.0f KB (×%d slices)", float64(len(pngCap))/1024.0, len(entries))
			rq.printUI(uiParams{
				bgColor:    *bgColor,
				pageHeight: fmt.Sprintf("%d PX", h),
				imgSize:    sSize,
				sliceList:  entries,
			})
			log.Printf("%s Done, %d slices for %s\n", rq.r.RemoteAddr, len(entries), rq.url)
			return
		}
		// Page shorter than one slice — fall through to single image.
	}

	// ── Single-image path (original behaviour) ────────────────────────────
	imgPath := fmt.Sprintf("/img/%s.%s", seq, ext)
	mapPath := fmt.Sprintf("/map/%s.map", seq)
	wrpCach.addMap(mapPath, *rq)

	encoded, err := rq.encodeImage(fullImg)
	if err != nil {
		log.Printf("%s Failed to encode %s: %s\n", rq.r.RemoteAddr, rq.imgType, err)
		fmt.Fprintf(rq.w, "<BR>Unable to encode image:<BR>%s<BR>\n", err)
		return
	}
	wrpCach.addImg(imgPath, *bytes.NewBuffer(encoded))

	b := fullImg.Bounds()
	iW, iH := b.Max.X, b.Max.Y
	sSize := fmt.Sprintf("%.0f KB", float32(len(encoded))/1024.0)
	log.Printf("%s Encoded %s: %s, Size: %s, Res: %dx%d\n",
		rq.r.RemoteAddr, rq.imgType, imgPath, sSize, iW, iH)

	if rq.proxy {
		rq.w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(rq.w,
			"<HTML><HEAD>%s<TITLE>%s</TITLE></HEAD><BODY BGCOLOR=\"%s\">"+
				"<A HREF=\"%s\"><IMG SRC=\"%s\" BORDER=\"0\" WIDTH=\"%d\" HEIGHT=\"%d\" ISMAP></A>"+
				"</BODY></HTML>",
			rq.baseTag(), rq.url, *bgColor, mapPath, imgPath, iW, iH)
	} else {
		rq.printUI(uiParams{
			bgColor:    *bgColor,
			pageHeight: fmt.Sprintf("%d PX", h),
			imgSize:    sSize,
			imgURL:     imgPath,
			mapURL:     mapPath,
			imgWidth:   iW,
			imgHeight:  iH,
		})
	}
	log.Printf("%s Done with capture for %s\n", rq.r.RemoteAddr, rq.url)
}

// sliceResult holds the encoded output of one strip for ordered collection.
type sliceResult struct {
	n       int
	imgPath string
	mapPath string
	encoded []byte
	stripH  int
	yOffset int
	err     error
}

// sliceAndCache cuts fullImg into strips of *sliceHeight pixels tall, encodes
// each strip concurrently, then stores results in the cache.  Every strip gets
// its own /map/ entry with sliceYOffset set so mapServer can correct clicks.
func (rq *wrpReq) sliceAndCache(fullImg image.Image, seq, ext string) []sliceEntry {
	b := fullImg.Bounds()
	totalH := b.Max.Y
	totalW := b.Max.X
	sh := *sliceHeight

	// Pre-compute strip boundaries so we can launch all goroutines at once.
	type stripBounds struct{ y, bottom int }
	var bounds []stripBounds
	for y := 0; y < totalH; y += sh {
		bottom := y + sh
		if bottom > totalH {
			bottom = totalH
		}
		bounds = append(bounds, stripBounds{y, bottom})
	}

	results := make([]sliceResult, len(bounds))
	var wg sync.WaitGroup

	for i, bnd := range bounds {
		wg.Add(1)
		go func(n, y, bottom int) {
			defer wg.Done()
			stripH := bottom - y

			// Copy strip pixels into a fresh NRGBA — works regardless of
			// source image type (NRGBA, YCbCr, etc.).
			strip := image.NewNRGBA(image.Rect(0, 0, totalW, stripH))
			draw.Draw(strip, strip.Bounds(), fullImg, image.Point{0, y}, draw.Src)

			encoded, err := rq.encodeImage(strip)
			results[n] = sliceResult{
				n:       n,
				imgPath: fmt.Sprintf("/slice/%s_%d.%s", seq, n, ext),
				mapPath: fmt.Sprintf("/map/%s_%d.map", seq, n),
				encoded: encoded,
				stripH:  stripH,
				yOffset: y,
				err:     err,
			}
		}(i, bnd.y, bnd.bottom)
	}
	wg.Wait()

	// Collect results in order and populate the cache.
	var entries []sliceEntry
	for _, r := range results {
		if r.err != nil {
			log.Printf("Slice %d encode error: %s", r.n, r.err)
			continue
		}
		wrpCach.addSlice(r.imgPath, cachedSlice{
			buf:     *bytes.NewBuffer(r.encoded),
			yOffset: r.yOffset,
		})
		rqCopy := *rq
		rqCopy.sliceYOffset = r.yOffset
		wrpCach.addMap(r.mapPath, rqCopy)
		entries = append(entries, sliceEntry{
			ImgURL: r.imgPath,
			MapURL: r.mapPath,
			Width:  totalW,
			Height: r.stripH,
		})
		log.Printf("Slice %d: y=%d-%d path=%s size=%.0fKB",
			r.n, r.yOffset, r.yOffset+r.stripH, r.imgPath, float64(len(r.encoded))/1024.0)
	}
	return entries
}

// mapServer handles ISMAP clicks.  For sliced pages it adds sliceYOffset to
// the raw y from the DSi browser before dispatching the mouse click.
func mapServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)
	rq, ok := wrpCach.getMap(r.URL.Path)
	rq.r = r
	rq.w = w
	if !ok {
		fmt.Fprintf(w, "Unable to find map %s\n", r.URL.Path)
		log.Printf("Unable to find map %s\n", r.URL.Path)
		return
	}
	n, err := fmt.Sscanf(r.URL.RawQuery, "%d,%d", &rq.mouseX, &rq.mouseY)
	if err != nil || n != 2 {
		fmt.Fprintf(w, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", r.RemoteAddr, n, err)
		return
	}

	// Correct y for sliced pages: raw y is relative to the strip top, but
	// Chromium needs y relative to the full viewport.
	if rq.sliceYOffset > 0 {
		log.Printf("%s Slice y-offset: raw=%d + offset=%d = %d\n",
			r.RemoteAddr, rq.mouseY, rq.sliceYOffset, int64(rq.sliceYOffset)+rq.mouseY)
		rq.mouseY += int64(rq.sliceYOffset)
	}

	log.Printf("%s WrpReq from ISMAP: %+v\n", r.RemoteAddr, rq)
	if len(rq.url) < 4 {
		rq.printUI(uiParams{})
		return
	}
	rq.navigate()
	if rq.proxy {
		chromedp.Run(ctx, waitForRender())
		var loc string
		chromedp.Run(ctx, chromedp.Location(&loc))
		loc = strings.Replace(loc, "https://", "http://", 1)
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}
	rq.captureScreenshot()
}

// imgServerMap serves full-page images from /img/*.
func imgServerMap(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s IMG Request for %s\n", r.RemoteAddr, r.URL.Path)
	imgBuf, ok := wrpCach.getImg(r.URL.Path)
	if !ok || imgBuf.Bytes() == nil {
		fmt.Fprintf(w, "Unable to find image %s\n", r.URL.Path)
		log.Printf("%s Unable to find image %s\n", r.RemoteAddr, r.URL.Path)
		return
	}
	serveImgBuf(w, r.URL.Path, imgBuf.Bytes())
}

// imgServerSlice serves individual slice images from /slice/*.
func imgServerSlice(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s SLICE Request for %s\n", r.RemoteAddr, r.URL.Path)
	sl, ok := wrpCach.getSlice(r.URL.Path)
	if !ok {
		fmt.Fprintf(w, "Unable to find slice %s\n", r.URL.Path)
		log.Printf("%s Unable to find slice %s\n", r.RemoteAddr, r.URL.Path)
		return
	}
	// Signal that a slice is being served. clear() will Wait() on this
	// before wiping the cache so the DSi never gets a 404 mid-page-load.
	wrpCach.sliceWg.Add(1)
	defer wrpCach.sliceWg.Done()
	serveImgBuf(w, r.URL.Path, sl.buf.Bytes())
}

func serveImgBuf(w http.ResponseWriter, path string, data []byte) {
	var ct string
	switch {
	case strings.HasSuffix(path, ".gif"):
		ct = "image/gif"
	case strings.HasSuffix(path, ".png"):
		ct = "image/png"
	default:
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(data)
	w.(http.Flusher).Flush()
}
