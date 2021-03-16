package main

import (
	"bytes"
	"container/ring"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/getsentry/raven-go"
	"github.com/golang/protobuf/proto"
	"github.com/ttgmpsn/mira"
	"github.com/ttgmpsn/mira/models"
	"github.com/turnage/graw"
	reddit1 "github.com/turnage/graw/reddit"
	"github.com/turnage/redditproto"
	"io/ioutil"
	"mvdan.cc/xurls/v2"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var auddToken = ""
var ravenDSN = "https://...:...@sentry.io/..."

var triggers = []string{"what's the song", "what is the song", "what's this song", "what is this song",
	"what song is playing", "what song is this", "what the song is playing",  "what the song is this",
	"recognizesong", "auddbot", "u/find-song"}

var replySettings = map[string]bool{
	"mentionLinks": true,
	"commentLinks": false,
	"postLinks": false,
	"mentionReplyAlways": true,
	"commentReplyAlways": false,
	"postReplyAlways": false,
}

var commentsCounter uint64 = 0
var postsCounter uint64 = 0

var markdownRegex = regexp.MustCompile(`\[[^][]+]\((https?://[^()]+)\)`)
var rxStrict = xurls.Strict()

func linksFromBody(body string) [][]string{
	results := markdownRegex.FindAllStringSubmatch(body, -1)
	//if len(results) == 0 {
		plaintextUrls := rxStrict.FindAllString(body, -1)
		for i := range plaintextUrls {
			plaintextUrls[i] = strings.ReplaceAll(plaintextUrls[i], "\\", "")
			results = append(results, []string{plaintextUrls[i], plaintextUrls[i]})
		}
	//}
	return results
}

func getJSON(URL string, v interface{}) error {
	resp, err := http.Get(URL)
	defer resp.Body.Close()
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(body, v)
	return err
}

func (r *auddBot) GetLinkFromComment(mention *reddit1.Message, commentsTree []*models.Comment, post *models.Post) (string, error) {
	// Check:
	// - first for the links in the comment
	// - then for the links in the comment to which it was a reply
	// - then for the links in the post
	var resultUrl string
	if post != nil {
		resultUrl = post.URL
	}
	/*if strings.Contains(resultUrl, "reddit.com/rpan") {
		s := strings.Split(resultUrl, "/")
		jsonUrl := "https://strapi.reddit.com/videos/t3_" + s[len(s)-1]
		var page RedditStreamJSON
		err := getJSON(jsonUrl, &page)
		if err != nil {
			return "", err
		}
		resultUrl = page.Data.Stream.HlsURL
		if resultUrl == "" {
			return "", fmt.Errorf("got an empty URL of the live stream HLS")
		}
		return resultUrl, nil
	}*/
	if len(commentsTree) > 0 {
		if commentsTree[0] != nil {
			if resultUrl == "" {
				//resultUrl = commentsTree[0].r2.LinkURL
			}
			mention = commentToMessage(commentsTree[0])
			if len(commentsTree) > 1 {
				commentsTree = commentsTree[1:]
			} else {
				commentsTree = make([]*models.Comment, 0)
			}
		}
	} else {
		if mention == nil {
			if post == nil {
				return "", fmt.Errorf("mention, commentsTree, and post are all nil")
			}
			mention = &reddit1.Message{
				Context: post.Permalink,
				Body: post.Selftext,
			}
		}
	}
	if resultUrl == "" {
		j, _ := json.Marshal(post)
		fmt.Printf("Got a post that's not a link or an empty post (https://www.reddit.com%s, %s)\n", mention.Context, string(j))
		//return "", fmt.Errorf("got a post without any URL (%s)", string(j))
		resultUrl = "https://www.reddit.com"+ mention.Context
	}
	if strings.Contains(resultUrl, "reddit.com/") || resultUrl == "" {
		if post != nil {
			if strings.Contains(post.Selftext, "https://reddit.com/link/"+post.ID+"/video/") {
				s := strings.Split(post.Selftext, "https://reddit.com/link/"+post.ID+"/video/")
				s = strings.Split(s[1], "/")
				resultUrl = "https://v.redd.it/" + s[0] + "/"
			}
		}
	}
	// Use links:
	results := linksFromBody(mention.Body) // from the comment
	if len(commentsTree) > 0 {
		if commentsTree[0] != nil {
			results = append(results, linksFromBody(commentsTree[0].Body)...) // from the comment it's a reply to
		} else {
			capture(fmt.Errorf("commentTree shouldn't contain an r2 entry at this point! %v", commentsTree))
		}
	}
	if !strings.Contains(resultUrl, "reddit.com/") && resultUrl != "" {
		results = append(results, []string{resultUrl, resultUrl}) // that's the post link
	}
	if post != nil {
		results = append(results, linksFromBody(post.Selftext)...) // from the post body
	}
	if len(results) != 0 {
		// And now get the first link that's OK
		fmt.Println("Parsed from the text:", results)
		for u := range results {
			if strings.HasPrefix(results[u][1], "/") {
				continue
			}
			if strings.HasPrefix(results[u][1], "https://www.reddit.com/") {
				continue
			}
			resultUrl = results[u][1]
			break
		}
	}

	if strings.Contains(resultUrl, "reddit.com/") {
		jsonUrl := resultUrl + ".json"
		var page RedditPageJSON
		err := getJSON(jsonUrl, &page)
		if !capture(err) {
			if len(page) > 0 {
				if len(page[0].Data.Children) > 0 {
					if strings.Contains(page[0].Data.Children[0].Data.URL, "v.redd.it") {
						resultUrl = page[0].Data.Children[0].Data.URL
					} else {
						if len(page[0].Data.Children[0].Data.MediaMetadata) > 0 {
							for s := range page[0].Data.Children[0].Data.MediaMetadata {
								resultUrl = "https://v.redd.it/" + s + "/"
							}
						}
					}
				}
			}
		}
	}
	if strings.Contains(resultUrl, "vocaroo.com/") || strings.Contains(resultUrl, "https://voca.ro/") {
		s := strings.Split(resultUrl, "/")
		resultUrl = "https://media1.vocaroo.com/mp3/" + s[len(s)-1]
	}
	if strings.Contains(resultUrl, "https://v.redd.it/") {
		resultUrl += "/DASH_audio.mp4"
	}
	if strings.Contains(resultUrl, "https://reddit.com/link/") {
		capture(fmt.Errorf("got an unparsed URL, %s", resultUrl))
	}
	// ToDo: parse the the body and check if there's a timestamp; add it to the t URL parameter
	// (maybe, only when there's no t parameter in the url?)
	return resultUrl, nil
}

func (r *auddBot) GetVideoLink(mention *reddit1.Message, comment *models.Comment) (string, error) {
	var post *models.Post
	if mention == nil && comment == nil {
		return "", fmt.Errorf("empty mention and comment")
	}
	var parentId models.RedditID
	commentsTree := make([]*models.Comment, 0)
	if mention != nil {
		parentId = models.RedditID(mention.ParentID)
	} else {
		parentId = comment.ParentID
		commentsTree = append(commentsTree, comment)
	}
	for ; parentId != ""; {
		//posts, comments, _, _, err := r.client.Listings.Get(context.Background(), parentId)
		parent, err := r.r.SubmissionInfoID(parentId)
		//r.bot.Listing(parentId, )
		j, _ := json.Marshal(parent)
		fmt.Println("parent:", string(j))
		//parentId = ""
		if err != nil {
			return "", err
		}
		if p, ok := parent.(*models.Post); ok {
			post = p
			break
		}
		if c, ok := parent.(*models.Comment); ok {
			commentsTree = append(commentsTree, c)
			parentId = c.ParentID
		} else {
			return "", fmt.Errorf("got a result that's neither a post nor a comment, parent ID %s, %v",
				parentId, parent)
		}
	}
	// ToDo: find out why Reddit sometimes doesn't give us the post and fix
	return r.GetLinkFromComment(mention, commentsTree, post)
}

func GetSkipFromLink(resultUrl string) int {
	skip := -1
	u, err := url.Parse(resultUrl)
	if err == nil {
		if t := u.Query().Get("t"); t != "" {
			t = strings.ReplaceAll(t, "s", "")
			tInt := 0
			if strings.Contains(t, "m") {
				s := strings.Split(t, "m")
				tsInt, _ := strconv.Atoi(s[1])
				tInt += tsInt
				if strings.Contains(s[0], "h") {
					s := strings.Split(s[0], "h")
					if tmInt, err := strconv.Atoi(s[1]); !capture(err) {
						tInt += tmInt * 60
					}
					if thInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += thInt * 60 * 60
					}
				} else {
					if tmInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += tmInt * 60
					}
				}
			} else {
				if tsInt, err := strconv.Atoi(t); !capture(err) {
					tInt = tsInt
				}
			}
			skip -= tInt / 18
			fmt.Println("skip:", skip)
		}
	}
	return skip
}

func GetReply(result []audd.RecognitionEnterpriseResult, withLinks bool) string {
	links := map[string]bool{}
	texts := make([]string, 0)
	for _, results := range result {
		if len(results.Songs) == 0 {
			capture(fmt.Errorf("enterprise response has a result without any songs"))
		}
		for _, song := range results.Songs {
			if song.SongLink != "" {
				if _, exists := links[song.SongLink]; exists { // making sure this song isn't a duplicate
					continue
				}
				links[song.SongLink] = true
			}
			album := ""
			label := ""
			if song.Title != song.Album && song.Album != "" {
				album = "Album: ^(" + song.Album + "). "
			}
			if song.Artist != song.Label && song.Label != "Self-released"  && song.Label != "" {
				label = " by ^(" + song.Label + ")"
			}
			score := strconv.Itoa(song.Score) + "%"
			text := fmt.Sprintf("[**%s** by %s](%s) (%s; confidence: ^(%s)))\n\n%sReleased on ^(%s)%s.",
				song.Title, song.Artist, song.SongLink, song.Timecode, score, album, song.ReleaseDate, label)
			if !withLinks {
				text = fmt.Sprintf("**%s** by %s (%s; confidence: ^(%s)))\n\n%sReleased on ^(%s)%s.",
					song.Title, song.Artist, song.Timecode, score, album, song.ReleaseDate, label)
			}
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return ""
	}
	response := texts[0]
	if len(texts) > 1 {
		response = "Recognized multiple songs:"
		for i, text := range texts {
			response += fmt.Sprintf("\n\n%d. %s", i+1, text)
		}
	}
	return response
}

func (r *auddBot) HandleQuery(mention *reddit1.Message, comment *models.Comment, post *models.Post) {
	var resultUrl, t, parentID, body string
	var err error
	if mention != nil {
		t, parentID, body = "mention", mention.Name, mention.Body
	}
	if comment != nil {
		t, parentID, body = "comment", string(comment.GetID()), comment.Body
	}
	if post != nil {
		t, parentID, body = "post", string(post.GetID()), post.Selftext
		resultUrl, err = r.GetLinkFromComment(nil, nil, post)
	} else {
		resultUrl, err = r.GetVideoLink(mention, comment)
	}
	if capture(err) {
		return
	}
	body = strings.ToLower(body)
	if strings.HasSuffix(resultUrl, ".m3u8") {
		//ToDo: recognize music from live streams
		fmt.Println("Got a livestream", resultUrl)
		return
	}
	skip := GetSkipFromLink(resultUrl)
	fmt.Println(resultUrl)
	limit := 2
	if strings.Contains(resultUrl, "v.redd.it") {
		limit = 3
	}
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets":"true", "skip":strconv.Itoa(skip), "limit":strconv.Itoa(limit)})
	if capture(err) {
		return
	}
	links := []string{
		"[GitHub](https://github.com/AudDMusic/RedditBot) " +
			"[^(new issue)](https://github.com/AudDMusic/RedditBot/issues/new)",
		"[Donate](https://www.patreon.com/audd)",
		"[Feedback](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music%20recognition)",
	}
	donateLink := 1
	if len(result) == 0 {
		if !replySettings[t + "ReplyAlways"] &&
			!strings.Contains(body, "u/recognizesong") && !strings.Contains(body, "u/auddbot") {
			fmt.Println("No result")
			return
		}
		links = append(links[:donateLink], links[donateLink:]...)
	}
	footer := "\n\n" + strings.Join(links, " | ")
	withLinks := (strings.Contains(body, "u/recognizesong") || replySettings[t + "Links"]) &&
		!strings.Contains(body, "without links") && !strings.Contains(body, "/wl")
	response := GetReply(result, withLinks)
	if response == "" {
		timestamp := skip*-1 - 1
		timestamp *= 18
		response = fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from the [link](%s) at %d-%d seconds.",
			resultUrl, timestamp, timestamp+limit*18)
		if strings.Contains(resultUrl, "https://www.reddit.com/") {
			response = "Sorry, I couldn't get the video URL from the post or your comment."
		}
	}
	if withLinks {
		response += footer
	}
	fmt.Println(response)
	if strings.Contains(body, "u/recognizeSong") {
		_, err = r.r.ReplyWithID(parentID, response)
	} else {
		_, err = r.r2.ReplyWithID(parentID, response)
	}
	capture(err)
}

func (r *auddBot) Mention(p *reddit1.Message) error {
	// ToDo: find out why we don't get all the mentions
	// (e.g., https://www.reddit.com/r/NameThatSong/comments/m5l84h/chirptuneish_something_that_would_be_in_mo3/gr0jpc2/?utm_source=reddit&utm_medium=web2x&context=3)
	// Possibly, we don't get replies to our previous comments?
	j, _ := json.Marshal(p)
	fmt.Println("Got a mention", string(j))
	if !p.New {
		return nil
	}
	capture(r.r.ReadMessage(p.Name))
	r.HandleQuery(p, nil, nil)
	return nil
}

func (r *auddBot) Comment(p *models.Comment) {
	//fmt.Print("c") // why? to test the amount of new comments on Reddit!
	atomic.AddUint64(&commentsCounter, 1)
	//return nil
	compare := strings.ToLower(p.Body)
	trigger := false
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	if !trigger {
		return
	}
	j, _ := json.Marshal(p)
	fmt.Println("Got a comment", string(j))
	r.HandleQuery(nil, p, nil)
	return
}

func (r *auddBot) Post(p *models.Post)  {
	atomic.AddUint64(&postsCounter, 1)
	compare := strings.ToLower(p.Selftext)
	trigger := false
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	compare = strings.ToLower(p.Title)
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	if !trigger {
		return
	}
	j, _ := json.Marshal(p)
	fmt.Println("Got a post", string(j))
	r.HandleQuery(nil, nil, p)
	return
}

type auddBot struct {
	bot  reddit1.Bot
	audd *audd.Client
	r    *mira.Reddit
	r2    *mira.Reddit
}

func getReddit2Credentials(filename string) *mira.Reddit {
	buf, err := ioutil.ReadFile(filename)
	if capture(err) {
		panic(err)
	}
	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if capture(err) {
		panic(err)
	}
	r := mira.Init(mira.Credentials{
		ClientID:     *agent.ClientId,
		ClientSecret: *agent.ClientSecret,
		Username:     *agent.Username,
		Password:     *agent.Password,
		UserAgent:    *agent.UserAgent,
	})
	err = r.LoginAuth()
	if err != nil {
		panic(err)
	}
	if capture(err) {
		panic(err)
	}
	rMeObj, err := r.Me().Info()
	if err != nil {
		panic(err)
	}
	rMe, _ := rMeObj.(*models.Me)
	fmt.Printf("You are now logged in, /u/%s\n", rMe.Name)
	return r
}
func getReddit1Credentials(filename string) reddit1.Bot {
	buf, err := ioutil.ReadFile(filename)
	if capture(err) {
		panic(err)
	}
	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if capture(err) {
		panic(err)
	}
	bot, err := reddit1.NewBotFromAgentFile(filename, 0)
	if capture(err) {
		panic(err)
	}
	return bot
}

func main() {
	go func() {
		for {
			fmt.Println("Comments:", atomic.LoadUint64(&commentsCounter), "posts:", atomic.LoadUint64(&postsCounter))
			<- time.NewTicker(time.Minute).C
		}
	}()
	//client.Stream.Posts("")

	handler := &auddBot{
		bot: getReddit1Credentials("RecognizeSong.agent"),
		audd: audd.NewClient(auddToken),
		r: getReddit2Credentials("RecognizeSong.agent"),
		r2: getReddit2Credentials("auddbot.agent"),
	}
	handler.audd.SetEndpoint(audd.EnterpriseAPIEndpoint) // See https://docs.audd.io/enterprise

	cfg := graw.Config{Mentions: true}
	_, wait, err := graw.Run(handler, handler.bot, cfg)
	if capture(err) {
		panic(err)
	}

	postsStream, err := streamSubredditPosts(handler.r2,"all")
	if err != nil {
		panic(err)
	}
	commentsStream, err := streamSubredditComments(handler.r2, "all")
	if err != nil {
		panic(err)
	}
	go func() {
		var s models.Submission
		var p *models.Post
		for s = range postsStream.C {
			if s == nil {
				fmt.Println("Stream was closed")
				return
			}
			go atomic.AddUint64(&postsCounter, 1)
			p = s.(*models.Post)
			go handler.Post(p)
		}
	}()
	go func() {
		var s models.Submission
		for s = range commentsStream.C {
			var c *models.Comment
			if s == nil {
				fmt.Println("Stream was closed")
				return
			}
			go atomic.AddUint64(&commentsCounter, 1)
			c = s.(*models.Comment)
			go handler.Comment(c)
		}
	}()
	capture(err)
	fmt.Println("started")
	fmt.Println("graw run failed: ", wait())
}

func streamSubredditPosts(c *mira.Reddit, name string) (*mira.SubmissionStream, error) {
	sendC := make(chan models.Submission, 100)
	s := &mira.SubmissionStream{
		C:     sendC,
		Close: make(chan struct{}),
	}
	_, err := c.Subreddit(name).Posts("new", "all", 1)
	if err != nil {
		return nil, err
	}
	var last models.RedditID
	go func() {
		sent := ring.New(100)
		for {
			select {
			case <-s.Close:
				return
			default:
			}
			posts, err := c.Subreddit(name).PostsAfter(last, 100)
			if err != nil {
				close(sendC)
				return
			}
			if len(posts) > 90 {
				fmt.Printf("got %d new posts", len(posts))
			}
			for i := len(posts) - 1; i >= 0; i-- {
				if ringContains(sent, posts[i].GetID()) {
					continue
				}
				sendC <- posts[i]
				sent.Value = posts[i].GetID()
				sent = sent.Next()
			}
			if len(posts) == 0 {
				last = ""
			} else if len(posts) > 2 {
				last = posts[1].GetID()
			}
			time.Sleep(15 * time.Second)
		}
	}()
	return s, nil
}


func streamSubredditComments(c *mira.Reddit, name string) (*mira.SubmissionStream, error) {
	sendC := make(chan models.Submission, 100)
	s := &mira.SubmissionStream{
		C:     sendC,
		Close: make(chan struct{}),
	}
	_, err := c.Subreddit(name).Posts("new", "all", 1)
	if err != nil {
		return nil, err
	}
	var last models.RedditID
	go func() {
		sent := ring.New(100)
		T := time.NewTicker(time.Millisecond * 1100)
		for {
			select {
			case <-s.Close:
				return
			default:
			}
			comments, err := c.Subreddit(name).CommentsAfter("new", last, 100)
			if err != nil {
				close(sendC)
				return
			}
			if len(comments) > 90 {
				fmt.Printf("got %d new comments", len(comments))
			}
			for i := len(comments) - 1; i >= 0; i-- {
				if ringContains(sent, comments[i].GetID()) {
					continue
				}
				//fmt.Print("a")
				sendC <- comments[i]
				sent.Value = comments[i].GetID()
				sent = sent.Next()
			}
			if len(comments) == 0 {
				last = ""
			} else if len(comments) > 2 {
				last = comments[1].GetID()
			}
			<-T.C
		}
	}()
	return s, nil
}

func ringContains(r *ring.Ring, n models.RedditID) bool {
	ret := false
	r.Do(func(p interface{}) {
		if p == nil {
			return
		}
		i := p.(models.RedditID)
		if i == n {
			ret = true
		}
	})
	return ret
}


func init() {
	err := raven.SetDSN(ravenDSN)
	if err != nil {
		panic(err)
	}
}

func capture(err error) bool {
	if err == nil {
		return false
	}
	_, file, no, ok := runtime.Caller(1)
	if ok {
		err = fmt.Errorf("%v from %s#%d", err, file, no)
	}
	packet := raven.NewPacket(err.Error(), raven.NewException(err, raven.GetOrNewStacktrace(err, 1, 3, nil)))
	go raven.Capture(packet, nil)
	fmt.Println(err.Error())
	return true
}

func commentToMessage(comment *models.Comment) *reddit1.Message {
	if comment == nil {
		return nil
	}
	return &reddit1.Message{
		ID:               comment.ID,
		Name:             string(comment.GetID()),
		CreatedUTC:       uint64(comment.CreatedUTC),
		Author:           comment.Author,
		Body:             comment.Body,
		BodyHTML:         comment.BodyHTML,
		Context:          comment.Permalink,
		ParentID:         string(comment.ParentID),
		Subreddit:        comment.Subreddit,
		WasComment:       true,
	}
}

type RedditPageJSON []struct {
	Data struct {
		Children []struct {
			Data struct {
				MediaMetadata         map[string]struct {
						Status  string `json:"status"`
						E       string `json:"e"`
						DashURL string `json:"dashUrl"`
						X       int    `json:"x"`
						Y       int    `json:"y"`
						HlsURL  string `json:"hlsUrl"`
						ID      string `json:"id"`
						IsGif   bool   `json:"isGif"`
					}								   `json:"media_metadata"`
				URL                      string        `json:"url"`
			} `json:"data"`
		} `json:"children"`
		After  interface{} `json:"after"`
		Before interface{} `json:"before"`
	} `json:"data"`
}

/*type RedditStreamJSON struct {
	Data          struct {
		Stream    struct {
			StreamID      string      `json:"stream_id"`
			HlsURL        string      `json:"hls_url"`
			PublishAt     int64       `json:"publish_at"`
			HlsExistsAt   int64       `json:"hls_exists_at"`
			Thumbnail     string      `json:"thumbnail"`
			Width         int         `json:"width"`
			Height        int         `json:"height"`
			UpdateAt      int64       `json:"update_at"`
			State         string      `json:"state"`
			DurationLimit int         `json:"duration_limit"`
			VodAccessible bool        `json:"vod_accessible"`
		} `json:"stream"`
	} `json:"data"`
}*/
