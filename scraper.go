package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxQueueSize is the maximum size of the channel, per user, that
// feeds files to downloaders. After that, scraping slows down due
// to the channel being blocked and the scraper having to wait for
// files to free up.
// TODO(Liru): Implement infinite channels or something similar.
const MaxQueueSize = 10000

var (
	// 匹配 "http://xx.media.tumblr.com/xxxx/tumblr_inline_xxxx" 的链接。
	// ！！！ 这里应该匹配"http://xx.media.tumblr.com/xxxx/tumblr_xxxx" 更合理吧。！！！
	//inlineSearch   = regexp.MustCompile(`(http:\/\/\d{2}\.media\.tumblr\.com\/\w{32}\/tumblr_inline_\w+\.\w+)`) // FIXME: Possibly buggy/unoptimized.
	inlineSearch   = regexp.MustCompile(`(http:\/\/\d{2}\.media\.tumblr\.com\/\w{32}\/tumblr_\w+\.\w+)`) // FIXME: Possibly buggy/unoptimized.
	videoSearch    = regexp.MustCompile(`"hdUrl":"(.*\/tumblr_\w+)"`)                                    // fuck it
	altVideoSearch = regexp.MustCompile(`source src="(.*tumblr_\w+)(?:\/\d+)?" type`)
	gfycatSearch   = regexp.MustCompile(`href="https?:\/\/(?:www\.)?gfycat\.com\/(\w+)`)
)

// PostParseMap maps tumblr post types to functions that search those
// posts for content.
var PostParseMap = map[string]func(Post) []File{
	"photo":   parsePhotoPost,
	"answer":  parseAnswerPost,
	"regular": parseRegularPost,
	"video":   parseVideoPost,
}

// TrimJS trims the javascript response received from Tumblr.
// The response starts with "var tumblr_api_read = " and ends with ";".
// We need to remove these to parse the response as JSON.
func TrimJS(c []byte) []byte {
	// The length of "var tumblr_api_read = " is 22.
	return c[22 : len(c)-1]
}

//下载每个帖子post里的资源
func parsePhotoPost(post Post) (files []File) {
	var id string //纯娱乐变量？
	//不忽略照片
	if !cfg.IgnorePhotos {
		if len(post.Photos) == 0 {
			f := newFile(post.PhotoURL)
			files = append(files, f)
			id = f.Filename
		} else {
			for _, photo := range post.Photos {
				f := newFile(photo.PhotoURL)
				files = append(files, f)
				id = f.Filename
			}
		}
	}
	//不忽略视频 GIF?
	if !cfg.IgnoreVideos {
		var slug string
		if len(id) > 26 {
			slug = id[:26]
		}
		files = append(files, getGfycatFiles(post.PhotoCaption, slug)...)
	}
	return
}

func parseAnswerPost(post Post) (files []File) {
	if !cfg.IgnorePhotos {
		for _, f := range inlineSearch.FindAllString(post.Answer, -1) {
			files = append(files, newFile(f))
		}
	}
	return
}

func parseRegularPost(post Post) (files []File) {
	if !cfg.IgnorePhotos {
		for _, f := range inlineSearch.FindAllString(post.RegularBody, -1) {
			files = append(files, newFile(f))
		}
	}
	return
}

func parseVideoPost(post Post) (files []File) {
	if !cfg.IgnoreVideos {
		post.Video = bytes.Replace(post.Video, []byte("\\"), []byte(""), -1) // 将链接里的\\全部删除
		regextest := videoSearch.FindStringSubmatch(string(post.Video))      //检查正则表达式合法性
		if regextest == nil {                                                // hdUrl is false. We have to get the other URL.
			regextest = altVideoSearch.FindStringSubmatch(string(post.Video))
		}

		// If it's still nil, it means it's another embedded video type, like Youtube, Vine or Pornhub.
		// In that case, ignore it and move on. Not my problem.
		if regextest == nil {
			return
		}
		videoURL := strings.Replace(regextest[1], `\`, ``, -1)

		// If there are problems with downloading video, the below part may be the cause.
		// videoURL = strings.Replace(videoURL, `/480`, ``, -1)
		videoURL += ".mp4"

		f := newFile(videoURL)
		files = append(files, f)

		// We slice from 0 to 24 because that's the length of the ID
		// portion of a tumblr video file.
		slug := f.Filename[:23]

		files = append(files, getGfycatFiles(post.VideoCaption, slug)...)
	}
	return
}

func parseDataForFiles(post Post) (files []File) {
	fn, ok := PostParseMap[post.Type]
	if ok {
		files = fn(post)
	}
	return
}

func makeTumblrURL(u *User, i int) *url.URL {

	base := fmt.Sprintf("http://%s.tumblr.com/api/read/json", u.name)

	tumblrURL, err := url.Parse(base)
	checkFatalError(err, "tumblrURL: ")

	vals := url.Values{}
	vals.Set("num", "50")
	vals.Add("start", strconv.Itoa((i-1)*50))
	// vals.Add("type", "photo")

	if u.tag != "" {
		vals.Add("tagged", u.tag)
	}

	tumblrURL.RawQuery = vals.Encode()
	return tumblrURL
}

func shouldFinishScraping(lim <-chan time.Time, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		select {
		case <-done:
			return true
		case <-lim:
			// We get a value from limiter, and proceed to scrape a page.
			return false
		}
	}
}

func scrape(u *User, limiter <-chan time.Time) <-chan File {

	var once sync.Once
	u.fileChannel = make(chan File, MaxQueueSize)
	//创建一个goroutine来遍历出所有的blog post
	go func() {

		done := make(chan struct{})
		// 匿函数。用于关闭通道。
		closeDone := func() { close(done) }
		var i, numPosts int

		// We need to put all of the following into a function because
		// Go evaluates params at defer instead of at execution.
		// That, and it beats writing `defer` multiple times.
		defer func() {
			u.finishScraping(i)
		}()

		for i = 1; ; i++ {
			if shouldFinishScraping(limiter, done) {
				return
			}

			tumblrURL := makeTumblrURL(u, i)

			showProgress(u.name, "is on page", i, "/", (numPosts/50)+1)

			var resp *http.Response
			var err error
			var contents []byte

			for {
				resp, err = http.Get(tumblrURL.String())

				// XXX: Ugly as shit. This could probably be done better.
				if err != nil {
					log.Println("http.Get:", u, err)
					continue
				}

				contents, err = ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Println("ReadAll:", u, err,
						"(", len(contents), "/", resp.ContentLength, ")")
					continue
				}
				err = resp.Body.Close()
				checkError(err)
				break
			}
			atomic.AddUint64(&gStats.bytesOverhead, uint64(len(contents)))

			// This is returned as pure javascript. We need to filter out the variable and the ending semicolon.
			contents = TrimJS(contents)

			var blog TumbleLog
			err = json.Unmarshal(contents, &blog)
			if err != nil {
				// Goddamnit tumblr, make a consistent API that doesn't
				// fucking return strings AND booleans in the same field

				log.Println("Unmarshal:", err)
			}

			numPosts = blog.TotalPosts

			u.scrapeWg.Add(1)

			defer u.scrapeWg.Done()
			//循环下载一篇blog里的所有文件。
			for _, post := range blog.Posts {
				id, err := post.ID.Int64()
				if err != nil {
					log.Println(err)
				}
				// 更新最大的post id.
				u.updateHighestPost(id)

				if !cfg.ForceCheck && id <= u.lastPostID {
					once.Do(closeDone)
					return
				}
				//下载blog post里的单个文件。有的blog post里会有多张图片。
				u.Queue(post)

			} // Done searching all posts on a page

			if len(blog.Posts) < 50 {
				break
			}

		} // loop that searches blog, page by page

	}() // Function that asynchronously adds all downloadables from a blog to a queue
	return u.fileChannel
}
