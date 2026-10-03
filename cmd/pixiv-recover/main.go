// SPDX-License-Identifier: GPL-3.0-only
//
// pixiv-recover — Try to recover recently deleted Pixiv images still present
// on the CDN.
//
// The program first asks whether the artwork itself is still reachable.  If it
// is, the original image URL reported by the public metadata endpoint is used
// directly.  Otherwise the upload time is inferred from neighbouring artwork
// IDs (or supplied manually) and the small set of possible original-image URLs
// is probed.  It does not bypass authentication or access control; it only
// downloads URLs that the Pixiv image CDN still serves publicly.
//
// This file is a Go port of the Python script pixiv_recover.py.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const userAgent = "Mozilla/5.0 (compatible; pixiv-recover/1.0)"

// maxPageScan 是 --max-pages 为 0 时的安全上限；正常情况下会在某页
// 所有候选都返回 404 时提前停止，不会被真的扫到上限。
const maxPageScan = 10000

// Endpoints are variables so tests can point them at a local server.
var (
	pixivURL = "https://www.pixiv.net"
	cdnURL   = "https://i.pximg.net"
)

// jst is the Japan Standard Time zone used by Pixiv's per-minute paths.
var jst = time.FixedZone("JST", 9*60*60)

// magic holds the leading byte signatures used to validate downloads.
var magic = map[string][][]byte{
	"jpg":  {{0xff, 0xd8, 0xff}},
	"jpeg": {{0xff, 0xd8, 0xff}},
	"png":  {{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}},
	"gif":  {{'G', 'I', 'F', '8', '7', 'a'}, {'G', 'I', 'F', '8', '9', 'a'}},
	"webp": {{'R', 'I', 'F', 'F'}},
}

// originalPathRe extracts the upload timestamp embedded in an original URL.
var originalPathRe = regexp.MustCompile(`/img/(\d{4})/(\d{2})/(\d{2})/(\d{2})/(\d{2})/(\d{2})/`)

// httpClient is shared across requests so that connections are reused.
var httpClient = &http.Client{
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

// Candidate describes one possible original-image URL for an artwork page.
type Candidate struct {
	Timestamp time.Time
	Page      int
	Ext       string
}

// URL builds the CDN URL for the candidate image of the given artwork.
func (c Candidate) URL(artworkID int) string {
	stamp := c.Timestamp.In(jst).Format("2006/01/02/15/04/05")
	return fmt.Sprintf("%s/img-original/img/%s/%d_p%d.%s", cdnURL, stamp, artworkID, c.Page, c.Ext)
}

// response wraps an http.Response with a cancel func for its timeout context.
type response struct {
	*http.Response
	cancel context.CancelFunc
}

// Close releases the request context and closes the body.
func (r *response) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	return r.Response.Body.Close()
}

// doGet issues a GET request with the Pixiv headers and a per-request timeout.
func doGet(ctx context.Context, url string, byteRange bool, timeout time.Duration) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", pixivURL+"/")
	if byteRange {
		req.Header.Set("Range", "bytes=0-31")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &response{Response: resp, cancel: cancel}, nil
}

// iso formats a time like Python's datetime.isoformat().
func iso(t time.Time) string {
	if t.Nanosecond() != 0 {
		return t.Format("2006-01-02T15:04:05.000000-07:00")
	}
	return t.Format("2006-01-02T15:04:05-07:00")
}

// parseISO parses the ISO-8601 timestamps returned by the Pixiv AJAX API.
func parseISO(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	value = strings.Replace(value, "Z", "+00:00", 1)
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// illustInfo holds the bits of artwork metadata this tool needs.
type illustInfo struct {
	createDate  time.Time
	originalURL string
	pageCount   int
}

// fetchIllust reads the public metadata endpoint for an artwork.
func fetchIllust(ctx context.Context, artworkID int, timeout time.Duration) (*illustInfo, error) {
	url := fmt.Sprintf("%s/ajax/illust/%d", pixivURL, artworkID)
	resp, err := doGet(ctx, url, false, timeout)
	if err != nil {
		return nil, err
	}
	defer resp.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		Error bool `json:"error"`
		Body  *struct {
			CreateDate string `json:"createDate"`
			PageCount  int    `json:"pageCount"`
			URLs       *struct {
				Original string `json:"original"`
			} `json:"urls"`
			UserIllusts map[string]*struct {
				CreateDate string `json:"createDate"`
			} `json:"userIllusts"`
		} `json:"body"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Error || payload.Body == nil {
		return nil, errors.New("pixiv api error")
	}

	body := payload.Body
	info := &illustInfo{pageCount: body.PageCount}
	// The top-level createDate is sometimes rounded to :00.  Embedded cards
	// normally retain the actual second, so prefer those.
	var values []string
	if card, ok := body.UserIllusts[strconv.Itoa(artworkID)]; ok && card != nil && card.CreateDate != "" {
		values = append(values, card.CreateDate)
	}
	if body.CreateDate != "" {
		values = append(values, body.CreateDate)
	}
	for _, value := range values {
		if parsed, ok := parseISO(value); ok {
			info.createDate = parsed.In(jst)
			break
		}
	}
	if body.URLs != nil {
		info.originalURL = body.URLs.Original
	}
	return info, nil
}

// artworkTimestamp returns the precise timestamp exposed for a live
// neighbouring artwork, converted to JST.
func artworkTimestamp(ctx context.Context, artworkID int, timeout time.Duration) (time.Time, error) {
	info, err := fetchIllust(ctx, artworkID, timeout)
	if err != nil {
		return time.Time{}, err
	}
	if info.createDate.IsZero() {
		return time.Time{}, errors.New("no createDate found")
	}
	return info.createDate, nil
}

// timestampFromURL reads the upload timestamp out of an original image URL.
func timestampFromURL(raw string) (time.Time, bool) {
	match := originalPathRe.FindStringSubmatch(raw)
	if match == nil {
		return time.Time{}, false
	}
	stamp := strings.Join(match[1:], "/")
	parsed, err := time.ParseInLocation("2006/01/02/15/04/05", stamp, jst)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// extFromURL returns the lower-cased extension of a URL path.
func extFromURL(raw string) string {
	ext := strings.ToLower(filepath.Ext(raw))
	return strings.TrimPrefix(ext, ".")
}

type neighbour struct {
	id    int
	stamp time.Time
}

// inferWindow queries artwork IDs around the target and returns the window of
// candidate upload times, bounded by the closest neighbours on each side.
func inferWindow(ctx context.Context, artworkID, radius int, timeout time.Duration) (time.Time, time.Time, []neighbour, error) {
	var ids []int
	for offset := -radius; offset <= radius; offset++ {
		if offset != 0 {
			ids = append(ids, artworkID+offset)
		}
	}

	workers := len(ids)
	if workers > 8 {
		workers = 8
	}
	sem := make(chan struct{}, workers)
	var (
		mu    sync.Mutex
		found []neighbour
		wg    sync.WaitGroup
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sem <- struct{}{}
			stamp, err := artworkTimestamp(ctx, id, timeout)
			<-sem
			if err != nil {
				return
			}
			mu.Lock()
			found = append(found, neighbour{id: id, stamp: stamp})
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })
	var before, after *neighbour
	for i := range found {
		if found[i].id < artworkID {
			before = &found[i]
		} else if found[i].id > artworkID {
			after = &found[i]
			break
		}
	}
	if before == nil || after == nil {
		return time.Time{}, time.Time{}, nil,
			errors.New("无法从相邻 ID 同时找到前后时间；请增大 --radius，或用 --minute 手动指定。")
	}

	start := before.stamp.Truncate(time.Second)
	end := after.stamp.Truncate(time.Second)
	if end.Before(start) || end.Sub(start) > 5*time.Minute {
		return time.Time{}, time.Time{}, nil,
			fmt.Errorf("推断出的时间窗口异常：%s .. %s", iso(start), iso(end))
	}
	return start, end, found, nil
}

// validHeader checks that data starts with the magic bytes of the given ext.
func validHeader(ext string, data []byte) bool {
	if ext == "webp" {
		return len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP"
	}
	prefixes, ok := magic[ext]
	if !ok {
		return false
	}
	for _, prefix := range prefixes {
		if bytes.HasPrefix(data, prefix) {
			return true
		}
	}
	return false
}

// probe asks the CDN for the first 32 bytes of a candidate URL.
func probe(ctx context.Context, artworkID int, cand Candidate, timeout time.Duration) bool {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := doGet(probeCtx, cand.URL(artworkID), true, timeout)
	if err != nil {
		return false
	}
	defer resp.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	buf := make([]byte, 32)
	n, _ := io.ReadFull(resp.Body, buf)
	return n > 0 && validHeader(cand.Ext, buf[:n])
}

// findPage probes every (timestamp, ext) combination for one page in parallel.
func findPage(ctx context.Context, artworkID int, timestamps []time.Time, page int, exts []string, workers int, timeout time.Duration) *Candidate {
	candidates := make([]Candidate, 0, len(timestamps)*len(exts))
	for _, stamp := range timestamps {
		for _, ext := range exts {
			candidates = append(candidates, Candidate{Timestamp: stamp, Page: page, Ext: ext})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if workers > len(candidates) {
		workers = len(candidates)
	}
	if workers < 1 {
		workers = 1
	}

	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan Candidate, 1)

	var (
		idx int64 = -1
		wg  sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := int(atomic.AddInt64(&idx, 1))
				if n >= len(candidates) {
					return
				}
				cand := candidates[n]
				if probe(probeCtx, artworkID, cand, timeout) {
					select {
					case found <- cand:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()

	select {
	case cand := <-found:
		return &cand
	default:
		return nil
	}
}

// secondsBetween expands a window into one timestamp per second (inclusive).
func secondsBetween(start, end time.Time) []time.Time {
	count := int(end.Sub(start) / time.Second)
	out := make([]time.Time, 0, count+1)
	for n := 0; n <= count; n++ {
		out = append(out, start.Add(time.Duration(n)*time.Second))
	}
	return out
}

// replaceFile renames src over dst, removing an existing dst first on
// platforms where os.Rename cannot overwrite (e.g. Windows).
func replaceFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	return os.Rename(src, dst)
}

// downloadURL saves url into outputDir/filename, validating the leading bytes
// and retrying on transient failures.
func downloadURL(ctx context.Context, url, outputDir, filename, ext string, timeout time.Duration, retries int) (string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}
	destination := filepath.Join(outputDir, filename)
	temporary := destination + ".part"

	if file, err := os.Open(destination); err == nil {
		var head [32]byte
		n, _ := file.Read(head[:])
		_ = file.Close()
		if n > 0 && validHeader(ext, head[:n]) {
			return destination, nil
		}
	}

	var lastErr error
	backoff := 1
	for attempt := 0; attempt <= retries; attempt++ {
		dlTimeout := timeout
		if dlTimeout < 30*time.Second {
			dlTimeout = 30 * time.Second
		}

		resp, err := doGet(ctx, url, false, dlTimeout)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				file, ferr := os.Create(temporary)
				if ferr == nil {
					_, cerr := io.Copy(file, resp.Body)
					_ = file.Close()
					_ = resp.Close()
					if cerr == nil {
						if rerr := replaceFile(temporary, destination); rerr != nil {
							lastErr = rerr
						} else {
							return destination, nil
						}
					} else {
						lastErr = cerr
					}
				} else {
					_ = resp.Close()
					lastErr = ferr
				}
			} else {
				_ = resp.Close()
				lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			}
		} else {
			lastErr = err
		}
		_ = os.Remove(temporary)

		if attempt < retries {
			secs := backoff
			if secs > 8 {
				secs = 8
			}
			time.Sleep(time.Duration(secs) * time.Second)
			backoff *= 2
		}
	}
	if lastErr == nil {
		lastErr = errors.New("download failed")
	}
	return "", lastErr
}

// downloadCandidate downloads the file described by a probed candidate.
func downloadCandidate(ctx context.Context, artworkID int, cand Candidate, outputDir string, timeout time.Duration, retries int) (string, error) {
	filename := fmt.Sprintf("%d_p%d.%s", artworkID, cand.Page, cand.Ext)
	return downloadURL(ctx, cand.URL(artworkID), outputDir, filename, cand.Ext, timeout, retries)
}

// downloadOriginal downloads a known original-image URL reported by the API.
func downloadOriginal(ctx context.Context, url string, artworkID, page int, outputDir string, timeout time.Duration, retries int) (string, error) {
	ext := extFromURL(url)
	if ext == "" {
		ext = "jpg"
	}
	filename := fmt.Sprintf("%d_p%d.%s", artworkID, page, ext)
	return downloadURL(ctx, url, outputDir, filename, ext, timeout, retries)
}

// minuteValue implements flag.Value for the --minute option.
type minuteValue struct {
	value *time.Time
}

func (m *minuteValue) String() string {
	if m.value == nil {
		return ""
	}
	return m.value.Format("2006-01-02T15:04")
}

func (m *minuteValue) Set(raw string) error {
	parsed, err := time.ParseInLocation("2006-01-02T15:04", raw, jst)
	if err != nil {
		return errors.New("格式应为 YYYY-MM-DDTHH:MM（日本时间）")
	}
	m.value = &parsed
	return nil
}

// promptLine prints a prompt and reads one trimmed line from stdin.
// The second return value is false when the input stream is exhausted.
func promptLine(reader *bufio.Reader, prompt string) (string, bool) {
	fmt.Print(prompt)
	text, err := reader.ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return "", false
		}
		if strings.TrimSpace(text) == "" {
			return "", false
		}
	}
	text = strings.TrimSpace(text)
	// 兼容来自管道/文件的带 BOM 输入（PowerShell 会写入 UTF-8 BOM 字节，
	// 某些终端则给出 UTF-16LE，即每个字符后跟一个 NUL 字节）。
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\x00", "")
	return strings.TrimSpace(text), true
}

// askArtworkID asks for the next artwork ID.  An empty answer (or end of
// input) signals that the interactive loop should stop.
func askArtworkID(reader *bufio.Reader) (int, bool) {
	for {
		value, ok := promptLine(reader, "请输入作品 ID（直接回车退出）：")
		if !ok {
			return 0, false
		}
		if value == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			fmt.Println("作品 ID 应为正整数，请重新输入。")
			continue
		}
		return parsed, true
	}
}

// askMinute asks for the upload minute; an empty answer means the timestamp
// should be inferred from neighbouring IDs instead.
func askMinute(reader *bufio.Reader) (*time.Time, bool) {
	prompt := "请输入投稿分钟（日本时间 YYYY-MM-DDTHH:MM，直接回车则用相邻 ID 推断）："
	for {
		value, ok := promptLine(reader, prompt)
		if !ok {
			return nil, false
		}
		if value == "" {
			return nil, true
		}
		var parsed minuteValue
		if err := parsed.Set(value); err != nil {
			fmt.Println("格式不正确，示例：2024-08-16T21:30；也可直接回车跳过。")
			continue
		}
		return parsed.value, true
	}
}

// stringList collects repeated --ext flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(raw string) error {
	*s = append(*s, raw)
	return nil
}

// intersperse reorders args so all flags precede the positionals, mimicking
// argparse's tolerant handling of interleaved options.
func intersperse(args []string) []string {
	var flags, pos []string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				next := args[i+1]
				if !(len(next) > 1 && next[0] == '-') {
					flags = append(flags, next)
					i++
				}
			}
			i++
			continue
		}
		pos = append(pos, a)
		i++
	}
	return append(flags, pos...)
}

// options holds the resolved settings used to process one artwork.
type options struct {
	radius   int
	maxPages int
	workers  int
	retries  int
	timeout  time.Duration
	exts     []string
	output   string
	minute   *time.Time
}

// process recovers every available page of one artwork.
func process(ctx context.Context, artworkID int, opts options) int {
	// 先看作品本身是否还能访问：能访问就直接下载，不必枚举时间戳。
	var (
		timestamps []time.Time
		directURL  string
	)
	// 默认不限页数：一直往后探测，直到某一页的所有候选都返回 404。
	pageLimit := opts.maxPages
	if pageLimit <= 0 {
		pageLimit = maxPageScan
	}
	if info, err := fetchIllust(ctx, artworkID, opts.timeout); err == nil {
		if ts, ok := timestampFromURL(info.originalURL); ok {
			timestamps = []time.Time{ts}
			directURL = info.originalURL
			fmt.Printf("作品 %d 仍可访问，使用原图地址：%s\n", artworkID, info.originalURL)
		} else if !info.createDate.IsZero() {
			timestamps = []time.Time{info.createDate.Truncate(time.Second)}
			fmt.Printf("作品 %d 仍可访问，精确投稿时间：%s\n", artworkID, iso(timestamps[0]))
		}
		if info.pageCount > 1 {
			fmt.Printf("元数据显示共 %d 页。\n", info.pageCount)
		}
	}

	if len(timestamps) == 0 {
		// 作品已不可访问，退回推断/手动时间窗口的枚举方式。
		var start, end time.Time
		if opts.minute != nil {
			start = *opts.minute
			end = start.Add(59 * time.Second)
			fmt.Printf("使用手动时间窗口：%s .. %s\n", iso(start), iso(end))
		} else {
			fmt.Printf("作品 %d 无法直接访问，正在通过相邻 ID 推断投稿时间……\n", artworkID)
			var neighbours []neighbour
			var err error
			start, end, neighbours, err = inferWindow(ctx, artworkID, opts.radius, opts.timeout)
			if err != nil {
				fmt.Fprintf(os.Stderr, "错误：%s\n", err)
				return 2
			}
			for _, n := range neighbours {
				fmt.Printf("  相邻作品 %d: %s\n", n.id, iso(n.stamp))
			}
			fmt.Printf("推断时间窗口：%s .. %s\n", iso(start), iso(end))
		}
		timestamps = secondsBetween(start, end)
	}

	var saved []string
	var follow []time.Time
	if directURL != "" {
		fmt.Println("作品可直接访问，跳过枚举，直接下载原图。")
		path, err := downloadOriginal(ctx, directURL, artworkID, 0, opts.output, opts.timeout, opts.retries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：下载 p0 失败：%s\n", err)
			return 1
		}
		saved = append(saved, path)
		follow = timestamps
	} else {
		fmt.Printf("探测 p0：%d 个候选 URL\n", len(timestamps)*len(opts.exts))
		first := findPage(ctx, artworkID, timestamps, 0, opts.exts, opts.workers, opts.timeout)
		if first == nil {
			fmt.Println("未命中：CDN 文件可能已清理，或文件名包含不可枚举的哈希。")
			return 1
		}
		fmt.Printf("命中：%s\n", first.URL(artworkID))
		path, err := downloadCandidate(ctx, artworkID, *first, opts.output, opts.timeout, opts.retries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：下载 p0 失败：%s\n", err)
			return 1
		}
		saved = append(saved, path)
		// 所有页共用一个时间戳；扩展名可能不同，仍需逐页探测。
		follow = []time.Time{first.Timestamp}
	}

	for page := 1; page < pageLimit; page++ {
		candidate := findPage(ctx, artworkID, follow, page, opts.exts, opts.workers, opts.timeout)
		if candidate == nil {
			fmt.Printf("p%d 未命中（404），停止探测。\n", page)
			break
		}
		path, err := downloadCandidate(ctx, artworkID, *candidate, opts.output, opts.timeout, opts.retries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：下载 p%d 失败：%s\n", candidate.Page, err)
			break
		}
		saved = append(saved, path)
		fmt.Printf("命中：%s\n", candidate.URL(artworkID))
	}

	fmt.Printf("完成，共保存 %d 张：\n", len(saved))
	for _, path := range saved {
		if abs, err := filepath.Abs(path); err == nil {
			fmt.Printf("  %s\n", abs)
		} else {
			fmt.Printf("  %s\n", path)
		}
	}
	return 0
}

func run(args []string) int {
	fs := flag.NewFlagSet("pixiv-recover", flag.ExitOnError)
	var minute minuteValue
	radius := fs.Int("radius", 8, "查询相邻 ID 的半径")
	maxPages := fs.Int("max-pages", 0, "最多尝试的页数（0 表示不限，一直探测到 404）")
	var extFlags stringList
	fs.Var(&extFlags, "ext", "要探测的文件扩展名，可重复指定（默认 jpg,png,gif,webp）")
	workers := fs.Int("workers", 12, "并发探测数")
	timeoutSecs := fs.Float64("timeout", 8.0, "单请求超时秒数")
	retries := fs.Int("retries", 3, "下载失败重试次数")
	output := fs.String("output", "recovered", "保存下载文件的目录")
	fs.Var(&minute, "minute", "已知投稿分钟（日本时间），格式 YYYY-MM-DDTHH:MM")

	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "用法: %s [选项] 作品ID\n\n枚举 Pixiv CDN URL，尝试找回近期删除的图片；未给出作品 ID 时进入交互模式。\n\n选项:\n", filepath.Base(os.Args[0]))
		fs.PrintDefaults()
	}

	if err := fs.Parse(intersperse(args)); err != nil {
		return 2
	}

	exts := []string(extFlags)
	if len(exts) == 0 {
		exts = []string{"jpg", "png", "gif", "webp"}
	}
	var unknown []string
	for _, ext := range exts {
		if _, ok := magic[ext]; !ok {
			unknown = append(unknown, ext)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fmt.Fprintf(os.Stderr, "不支持的扩展名：%s\n", strings.Join(unknown, ", "))
		return 2
	}

	opts := options{
		radius:   *radius,
		maxPages: *maxPages,
		workers:  *workers,
		retries:  *retries,
		timeout:  time.Duration(*timeoutSecs * float64(time.Second)),
		exts:     exts,
		output:   *output,
		minute:   minute.value,
	}

	rest := fs.Args()
	ctx := context.Background()
	if len(rest) < 1 {
		// 交互模式：处理完一个作品后继续询问下一个。
		fmt.Println("未检测到作品 ID，进入交互模式（每次输入一个 ID，直接回车结束）。")
		input := bufio.NewReader(os.Stdin)
		exitCode := 0
		for {
			artworkID, ok := askArtworkID(input)
			if !ok {
				break
			}
			current := opts
			if current.minute == nil {
				current.minute, _ = askMinute(input)
			}
			if code := process(ctx, artworkID, current); code != 0 {
				exitCode = code
			}
			fmt.Println()
		}
		return exitCode
	}

	artworkID, err := strconv.Atoi(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：作品 ID 应为整数，收到 %q\n", rest[0])
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintf(os.Stderr, "错误：无法识别的参数：%s\n", strings.Join(rest[1:], " "))
		return 2
	}
	return process(ctx, artworkID, opts)
}

func main() {
	os.Exit(run(os.Args[1:]))
}
