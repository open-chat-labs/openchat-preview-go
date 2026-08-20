package main

import (
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	// Registered for their DecodeConfig implementations only - we never decode
	// the pixels, just the dimensions out of the format header.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/PuerkitoBio/goquery"
	"github.com/dyatlov/go-opengraph/opengraph"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
	_ "golang.org/x/image/webp"
)

var whitelist = map[string]bool{
	"http://localhost:5001":  true,
	"https://oc.app":         true,
	"https://test.oc.app":    true,
	"https://webtest.oc.app": true,
	"http://tauri.localhost": true,
	"tauri://localhost":	  true,
}

type OGData struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	ImageAlt    string `json:"imageAlt,omitempty"`
	ImageWidth  uint64 `json:"imageWidth,omitempty"`
	ImageHeight uint64 `json:"imageHeight,omitempty"`
	BadResponse bool   `json:"badResponse,omitempty"`
	Error       string `json:"error,omitempty"`
}

var cache *lru.LRU[string, OGData]

func main() {
	var err error
	cache = lru.NewLRU[string, OGData](5000, nil, time.Hour)

	rateLimitBuckets = lru.NewLRU[string, *tokenBucket](maxRateLimitedIPs, nil, rateLimitIdleTTL)

	mux := http.NewServeMux()
	mux.Handle("/preview", rateLimitMiddleware(http.HandlerFunc(handlePreview)))

	go logMemoryUsage()

	log.Println("Proxy running on :5070")
	err = http.ListenAndServe(":5070", corsMiddleware(mux))
	if err != nil {
		log.Fatal(err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if whitelist[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else if origin != "" {
			http.Error(w, fmt.Sprintf("Origin %s is not permitted", origin), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const (
	// Per caller IP: a sustained rate with a burst allowance on top. A browser
	// opening a chat full of links fires a handful of previews at once, hence
	// the generous burst - the sustained rate is what stops a scripted caller
	// hammering us.
	rateLimitPerSecond = 5
	rateLimitBurst     = 30
	// Bound the limiter state: at most this many IPs are tracked and a bucket
	// idle for longer than the TTL is evicted.
	maxRateLimitedIPs = 10000
	rateLimitIdleTTL  = 10 * time.Minute
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

var (
	rateLimitMu      sync.Mutex
	rateLimitBuckets *lru.LRU[string, *tokenBucket]
)

// allowRequest refills the caller's bucket for the time elapsed since its last
// request and takes a token, returning false if there wasn't one to take.
func allowRequest(ip string) bool {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	now := time.Now()
	bucket, ok := rateLimitBuckets.Get(ip)
	if !ok {
		bucket = &tokenBucket{tokens: rateLimitBurst, last: now}
		rateLimitBuckets.Add(ip, bucket)
	}

	bucket.tokens += now.Sub(bucket.last).Seconds() * rateLimitPerSecond
	if bucket.tokens > rateLimitBurst {
		bucket.tokens = rateLimitBurst
	}
	bucket.last = now

	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// clientIP prefers the first X-Forwarded-For entry (we sit behind CloudFront)
// and falls back to the connecting address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !allowRequest(ip) {
			log.Println("Rate limiting", ip)
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error": "Too many requests"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rawURL := query.Get("url")
	if rawURL == "" {
		http.Error(w, `{"error": "URL parameter is required"}`, http.StatusBadRequest)
		return
	}

	callerOrigin := r.Header.Get("Origin")
	if callerOrigin == "" {
		callerOrigin = r.Header.Get("Referer")
	}

	if callerOrigin != "" {
		callerURL, err1 := url.Parse(callerOrigin)
		targetURL, err2 := url.Parse(rawURL)

		if err1 != nil || err2 != nil {
			log.Println("Failed to parse URL for origin check:", err1, err2)
			http.Error(w, `{"error": "Failed to parse URL for origin check"}`, http.StatusBadRequest)
			return
		}

		if targetURL.Scheme != "https" {
			http.Error(w, `{"error": "Only HTTPS URLs are supported"}`, http.StatusBadRequest)
			return
		}

		if callerURL.Scheme == targetURL.Scheme && callerURL.Host == targetURL.Host {
			msg := fmt.Sprintf("We cannot return meaningful metadata for internal links (yet): %s", rawURL)
			log.Println(msg)
			// Internal links are a permanent failure - don't cache since we check before cache lookup
			data := OGData{
				BadResponse: true,
				Error:       "Cannot return meaningful metadata for internal links",
			}
			w.Header().Set("Cache-Control", "public, max-age=86400") // 24 hours for client
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(data)
			return
		}
	}

	// Check cache first
	if data, ok := cache.Get(rawURL); ok {
		log.Println("Returning OpenGraph metadata from cache for", rawURL)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
		return
	}

	// Fetch new data
	data, err := fetchOGData(rawURL)
	if err != nil {
		log.Println("Error getting OpenGraph metadata", rawURL, err)
		// Create error response but still return 200
		data = OGData{
			BadResponse: true,
			Error:       fmt.Sprintf("OpenGraph metadata not available for %s", rawURL),
		}
	}

	// Cache the result
	cache.Add(rawURL, data)
	
	// Always return 200 with consistent caching headers (1 hour for client)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

// A realistic browser User-Agent plus Google's SOCS consent cookie. Together
// these get past YouTube's consent interstitial, though not necessarily its
// datacenter-IP bot check - hence the oEmbed fallback below.
const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

func isYouTubeURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return host == "youtube.com" || host == "www.youtube.com" ||
		host == "m.youtube.com" || host == "music.youtube.com" ||
		host == "youtu.be"
}

// extractYouTubeVideoID returns the video id from the various YouTube URL
// shapes (watch, youtu.be, shorts, live, embed), or "" if there isn't one.
func extractYouTubeVideoID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	if host == "youtu.be" {
		return strings.Trim(u.Path, "/")
	}
	if id := u.Query().Get("v"); id != "" {
		return id
	}
	for _, prefix := range []string{"/shorts/", "/live/", "/embed/"} {
		if rest, ok := strings.CutPrefix(u.Path, prefix); ok {
			return strings.Trim(rest, "/")
		}
	}
	return ""
}

type oEmbedResponse struct {
	Title           string `json:"title"`
	AuthorName      string `json:"author_name"`
	ThumbnailURL    string `json:"thumbnail_url"`
	ThumbnailWidth  uint64 `json:"thumbnail_width"`
	ThumbnailHeight uint64 `json:"thumbnail_height"`
}

func fetchYouTubeOEmbed(rawURL string) (OGData, error) {
	// Normalize to the watch form - oEmbed 404s on some URL shapes (e.g.
	// shorts) but always accepts watch?v=<id>. URLs with no extractable id
	// (channels, playlists, the oEmbed endpoint itself) have no video to
	// look up, so don't bother asking.
	id := extractYouTubeVideoID(rawURL)
	if id == "" {
		return OGData{}, fmt.Errorf("no video id in YouTube URL, skipping oEmbed")
	}
	endpoint := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape("https://www.youtube.com/watch?v="+id)

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return OGData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return OGData{}, fmt.Errorf("bad oEmbed status code: %d", resp.StatusCode)
	}

	var oe oEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oe); err != nil {
		return OGData{}, err
	}
	if oe.Title == "" {
		return OGData{}, fmt.Errorf("oEmbed response contained no title")
	}

	return OGData{
		Title:       oe.Title,
		Description: oe.AuthorName,
		Image:       oe.ThumbnailURL,
		ImageWidth:  oe.ThumbnailWidth,
		ImageHeight: oe.ThumbnailHeight,
	}, nil
}

// fetchYouTubeOGData first tries the page itself (which, when it works, includes
// the video description), then falls back to the oEmbed API which is not
// bot-walled but carries no description.
func fetchYouTubeOGData(rawURL string) (OGData, error) {
	data, err := fetchPageOGData(rawURL, true)
	if err == nil && data.Title != "" {
		return data, nil
	}
	log.Println("Direct YouTube fetch returned no OG data, falling back to oEmbed:", rawURL)
	return fetchYouTubeOEmbed(rawURL)
}

func isTwitterURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return host == "twitter.com" || host == "www.twitter.com" ||
		host == "x.com" || host == "www.x.com"
}

func toFxTwitterURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Host = "fxtwitter.com"
	return u.String()
}

func fetchOGData(rawURL string) (OGData, error) {
	if isTwitterURL(rawURL) {
		rawURL = toFxTwitterURL(rawURL)
		log.Println("Rewriting Twitter/X URL to fxtwitter:", rawURL)
	}

	var data OGData
	var err error
	if isYouTubeURL(rawURL) {
		data, err = fetchYouTubeOGData(rawURL)
	} else {
		data, err = fetchPageOGData(rawURL, false)
	}
	if err != nil {
		return data, err
	}

	// Callers drop the image entirely unless both dimensions are known, and
	// plenty of pages omit og:image:width / og:image:height. Non-browser
	// callers (the bot SDKs) can't measure the image themselves, so do it here.
	if data.Image != "" && (data.ImageWidth == 0 || data.ImageHeight == 0) {
		if w, h := measureImage(data.Image); w > 0 && h > 0 {
			data.ImageWidth, data.ImageHeight = w, h
		} else {
			log.Println("Could not determine image dimensions for", data.Image)
		}
	}

	return data, nil
}

const (
	imageFetchTimeout = 5 * time.Second
	// The dimensions live in the first few KB of every format we understand,
	// but a progressive JPEG can carry a lot of metadata ahead of its frame
	// header. This is a ceiling, not a target - we stop as soon as the header
	// decodes.
	maxImageHeaderBytes = 256 * 1024
)

// measureImage fetches just enough of the image to read its dimensions out of
// the format header. Everything about this is best effort: any failure returns
// 0, 0 and the caller simply leaves the dimensions unset.
func measureImage(rawURL string) (width uint64, height uint64) {
	// Decoders are fed truncated, untrusted bytes - don't let a panic in one
	// take down the request.
	defer func() {
		if r := recover(); r != nil {
			log.Println("Panic decoding image header for", rawURL, r)
			width, height = 0, 0
		}
	}()

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, 0
	}
	req.Header.Set("User-Agent", browserUserAgent)
	// A hint only - servers that ignore it are handled by the LimitReader.
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxImageHeaderBytes-1))

	client := http.Client{Timeout: imageFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, 0
	}

	cfg, _, err := image.DecodeConfig(io.LimitReader(resp.Body, maxImageHeaderBytes))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}

	return uint64(cfg.Width), uint64(cfg.Height)
}

func fetchPageOGData(rawURL string, browserHeaders bool) (OGData, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return OGData{}, err
	}
	if browserHeaders {
		req.Header.Set("User-Agent", browserUserAgent)
		req.Header.Set("Cookie", "SOCS=CAI")
	}

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return OGData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return OGData{}, fmt.Errorf("bad status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return OGData{}, err
	}

	og := opengraph.NewOpenGraph()
	err = og.ProcessHTML(strings.NewReader(string(body)))
	if err != nil {
		return OGData{}, err
	}

	altText := parseOGAltTag(body)

	var imageUrl string
	var imageWidth, imageHeight uint64
	if len(og.Images) > 0 {
		imageUrl = og.Images[0].URL
		imageWidth = og.Images[0].Width
		imageHeight = og.Images[0].Height
	}

	return OGData{
		Title:       og.Title,
		Description: og.Description,
		Image:       imageUrl,
		ImageAlt:    altText,
		ImageWidth:  imageWidth,
		ImageHeight: imageHeight,
		BadResponse: false,
	}, nil
}

func parseOGAltTag(html []byte) string {
    doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(html)))
    if err != nil {
        return ""
    }

    if tag := doc.Find(`meta[property="og:image:alt"]`).First(); tag != nil {
        if content, ok := tag.Attr("content"); ok {
            return content
        }
    }
    return ""
}

func logMemoryUsage() {
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("Heap: %.2f MB / %.2f MB\n", float64(m.HeapAlloc)/1024/1024, float64(m.HeapSys)/1024/1024)
		time.Sleep(60 * time.Second)
	}
}
