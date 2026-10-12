package control

import "testing"

func TestNormalizePlaybackPath(t *testing.T) {
	prefix := "/r/vmiss/key"
	for _, tc := range []struct{ in, want string }{
		{prefix + "/emby" + prefix + "/Videos/a/stream.mkv", "/emby/Videos/a/stream.mkv"},
		{prefix + prefix + "/Videos/a/stream.mkv", "/Videos/a/stream.mkv"},
		{prefix + "/emby/Users/id/Views", "/emby/Users/id/Views"},
		{prefix + "//emby/Users/AuthenticateByName", "/emby/Users/AuthenticateByName"},
		{prefix + "///Users/AuthenticateByName", "/Users/AuthenticateByName"},
		{prefix + "//emby" + prefix + "/Users/AuthenticateByName", "/emby/Users/AuthenticateByName"},
		{prefix + "/Videos/a%2Fb//stream.mkv", "/Videos/a%2Fb//stream.mkv"},
		{prefix + "/emby/r/other/key/Videos/a/stream.mkv", "/emby/r/other/key/Videos/a/stream.mkv"},
		{prefix + "/emby/r/vmiss/different/Videos/a/stream.mkv", "/emby/r/vmiss/different/Videos/a/stream.mkv"},
		{prefix + "/Videos/a%20b/stream.mkv", "/Videos/a%20b/stream.mkv"},
		{prefix, "/"},
	} {
		if got := normalizePlaybackPath(tc.in, prefix); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.in, got, tc.want)
		}
	}
}
