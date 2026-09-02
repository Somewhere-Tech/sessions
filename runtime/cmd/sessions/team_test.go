package main

import "testing"

func TestHomeRelativeUsesWindowsPathSemantics(t *testing.T) {
	application := &app{home: `C:\Users\Alice`}
	for _, testCase := range []struct {
		name string
		path string
		want string
	}{
		{name: "same home with different case", path: `c:\users\alice`, want: `~`},
		{name: "backslash child", path: `c:\USERS\ALICE\work\Sessions`, want: `~\work\Sessions`},
		{name: "forward slash child", path: `C:/Users/Alice/work/Sessions`, want: `~\work\Sessions`},
		{name: "extended path child", path: `\\?\C:\Users\Alice\work`, want: `~\work`},
		{name: "shared prefix is not a child", path: `C:\Users\Alice-tools\repo`, want: `C:\Users\Alice-tools\repo`},
		{name: "another drive is not a child", path: `D:\Users\Alice\repo`, want: `D:\Users\Alice\repo`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := application.homeRelative(testCase.path); got != testCase.want {
				t.Fatalf("homeRelative(%q) = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

func TestHomeRelativeKeepsUnixDisplayStyle(t *testing.T) {
	application := &app{home: "/Users/alice"}
	if got := application.homeRelative("/Users/alice/work/Sessions"); got != "~/work/Sessions" {
		t.Fatalf("homeRelative() = %q, want ~/work/Sessions", got)
	}
}
