package control

import (
	"net/http"
	"regexp"
	"strings"
)

// Only player operations are exposed on subscriber-owned routes. WebSocket
// upgrades are rejected because their payload cannot be identified as Emby.
var sourceReadPath = regexp.MustCompile(`(?i)^/(?:` +
	`system/(?:info(?:/public)?|ping)|branding/(?:configuration|css)|` +
	`users/(?:public|me|[a-z0-9_-]+(?:/(?:items(?:/(?:[a-z0-9_-]+|latest|resume))?|views|groupingoptions))?)|` +
	`items(?:/(?:[a-z0-9_-]+(?:/(?:playbackinfo|ancestors|children|localtrailers|specialfeatures|intros|similar|theme(?:songs|videos|media)|images(?:/(?:primary|art|backdrop|banner|logo|thumb|disc|box|boxrear|menu|screenshot)(?:/[0-9]+)?)?))?|latest|resume|counts|filters|filters2))?|` +
	`shows/[a-z0-9_-]+/(?:episodes|seasons)|shows/(?:nextup|upcoming)|` +
	`(?:genres|musicgenres|studios|persons|artists|years)(?:/[a-z0-9_-]+)?|artists/albumartists|search/hints|sessions|` +
	`videos/[a-z0-9_-]+/(?:stream(?:\.[a-z0-9]+)?|master\.m3u8|main\.m3u8|additionalparts|hls(?:1)?/[a-z0-9_-]+/[a-z0-9_.-]+|[a-z0-9_-]+/subtitles/[0-9]+(?:/[0-9]+)?/stream(?:\.[a-z0-9]+)?|[a-z0-9_-]+/subtitles/[0-9]+/subtitles\.m3u8)|` +
	`audio/[a-z0-9_-]+/(?:stream(?:\.[a-z0-9]+)?|master\.m3u8|main\.m3u8|hls(?:1)?/[a-z0-9_-]+/[a-z0-9_.-]+)|` +
	`__access_media/[a-z0-9_-]+|subscription/info)/?$`)

var sourceWritePath = regexp.MustCompile(`(?i)^/(?:` +
	`users/authenticatebyname|sessions/(?:logout|capabilities(?:/full)?|playing(?:/(?:progress|stopped))?)|` +
	`items/[a-z0-9_-]+/playbackinfo|users/[a-z0-9_-]+/(?:playeditems|favoriteitems)/[a-z0-9_-]+|` +
	`users/[a-z0-9_-]+/items/[a-z0-9_-]+/userdata)/?$`)

var sourceDeletePath = regexp.MustCompile(`(?i)^/(?:videos/activeencodings|users/[a-z0-9_-]+/(?:playeditems|favoriteitems)/[a-z0-9_-]+)/?$`)

func allowedUserSourceRequest(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return false
			}
		}
	}
	path := strings.ToLower(r.URL.Path)
	if strings.ContainsAny(path, "\\%") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	if strings.HasPrefix(path, "/emby/") {
		path = strings.TrimPrefix(path, "/emby")
	}
	switch r.Method {
	case "GET", "HEAD":
		return sourceReadPath.MatchString(path)
	case "POST":
		return sourceWritePath.MatchString(path)
	case "DELETE":
		return sourceDeletePath.MatchString(path)
	default:
		return false
	}
}
