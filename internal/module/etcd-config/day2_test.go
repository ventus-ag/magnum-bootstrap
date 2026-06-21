package etcdconfig

import "testing"

func TestParseEndpointStatus(t *testing.T) {
	// camelCase (etcdctl 3.5/3.6 default JSON).
	camel := `[{"Endpoint":"https://10.0.0.1:2379","Status":{"version":"3.6.10","dbSize":536870912,"leader":1,"raftIndex":42,"dbSizeInUse":268435456,"isLearner":false}}]`
	st, ok := parseEndpointStatus(camel)
	if !ok || st.dbSize != 536870912 || st.dbSizeInUse != 268435456 {
		t.Fatalf("camelCase parse failed: %+v ok=%t", st, ok)
	}

	// snake_case tolerance.
	snake := `[{"Status":{"db_size":1000,"db_size_in_use":400}}]`
	st, ok = parseEndpointStatus(snake)
	if !ok || st.dbSize != 1000 || st.dbSizeInUse != 400 {
		t.Fatalf("snake_case parse failed: %+v ok=%t", st, ok)
	}

	// Garbage / empty → not ok, no panic.
	for _, bad := range []string{"", "not json", "[]", `[{"Status":{}}]`} {
		if _, ok := parseEndpointStatus(bad); ok {
			t.Fatalf("expected parse failure for %q", bad)
		}
	}
}

func TestNeedsDefrag(t *testing.T) {
	const min = 128 * 1024 * 1024
	const ratio = 0.45
	cases := []struct {
		name              string
		dbSize, dbSizeUse int64
		want              bool
	}{
		{"below floor never defrags", 64 << 20, 1 << 20, false},
		{"large and very fragmented", 512 << 20, 100 << 20, true}, // ~80% free
		{"large but compact", 512 << 20, 500 << 20, false},        // ~2% free
		{"exactly at ratio", 200 << 20, 110 << 20, true},          // 45% free
		{"just under ratio", 200 << 20, 111 << 20, false},
		{"zero size", 0, 0, false},
		{"in-use exceeds size (defensive)", 200 << 20, 300 << 20, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsDefrag(c.dbSize, c.dbSizeUse, min, ratio); got != c.want {
				t.Fatalf("needsDefrag(%d,%d) = %t, want %t", c.dbSize, c.dbSizeUse, got, c.want)
			}
		})
	}
}

func TestVotingMemberCount(t *testing.T) {
	members := []etcdMember{
		{name: "m0", started: true, isLearner: false},
		{name: "m1", started: true, isLearner: false},
		{name: "m2", started: true, isLearner: true},   // learner — not a voter
		{name: "m3", started: false, isLearner: false}, // unstarted — not counted
	}
	if got := votingMemberCount(members); got != 2 {
		t.Fatalf("votingMemberCount = %d, want 2 (learner + unstarted excluded)", got)
	}
	if got := votingMemberCount(nil); got != 0 {
		t.Fatalf("votingMemberCount(nil) = %d, want 0", got)
	}
}

func TestParseAlarms(t *testing.T) {
	nospace, corrupt := parseAlarms("memberID:13803658152347727861 alarm:NOSPACE")
	if !nospace || corrupt {
		t.Fatalf("NOSPACE detection wrong: nospace=%t corrupt=%t", nospace, corrupt)
	}
	nospace, corrupt = parseAlarms("memberID:1 alarm:CORRUPT")
	if nospace || !corrupt {
		t.Fatalf("CORRUPT detection wrong: nospace=%t corrupt=%t", nospace, corrupt)
	}
	if n, c := parseAlarms(""); n || c {
		t.Fatalf("empty alarm list must be no alarms, got nospace=%t corrupt=%t", n, c)
	}
}
