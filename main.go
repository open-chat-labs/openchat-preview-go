package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/dyatlov/go-opengraph/opengraph"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/preview", handlePreview)

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
	// Normalize to the watch form where possible - oEmbed 404s on some URL
	// shapes (e.g. shorts) but always accepts watch?v=<id>.
	if id := extractYouTubeVideoID(rawURL); id != "" {
		rawURL = "https://www.youtube.com/watch?v=" + id
	}
	endpoint := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape(rawURL)

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

	if isYouTubeURL(rawURL) {
		return fetchYouTubeOGData(rawURL)
	}

	return fetchPageOGData(rawURL, false)
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
