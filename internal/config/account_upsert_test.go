package config

import "testing"

func TestUpsertAccount(t *testing.T) {
	cases := []struct {
		name       string
		list       []Account
		in         Account
		wantAction string
		wantID     string
		wantLen    int
		want       []Account
	}{
		{
			name:       "空列表追加",
			list:       nil,
			in:         Account{ID: "ext-1", UserID: "u1", ServiceToken: "t1", Ph: "p1", Active: true},
			wantAction: "added",
			wantID:     "ext-1",
			wantLen:    1,
		},
		{
			name: "同 ID 就地覆盖凭据",
			list: []Account{
				{ID: "main", UserID: "u1", ServiceToken: "old", Ph: "old", Active: false},
			},
			in:         Account{ID: "main", UserID: "u2", ServiceToken: "new", Ph: "new", Active: true},
			wantAction: "updated",
			wantID:     "main",
			wantLen:    1,
			want: []Account{
				{ID: "main", UserID: "u2", ServiceToken: "new", Ph: "new", Active: true},
			},
		},
		{
			name: "同 UserID 覆盖且保留原 ID",
			list: []Account{
				{ID: "ext-100", UserID: "u1", ServiceToken: "old", Ph: "old", Active: true},
			},
			in:         Account{ID: "ext-200", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
			wantAction: "updated",
			wantID:     "ext-100",
			wantLen:    1,
			want: []Account{
				{ID: "ext-100", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
			},
		},
		{
			name: "UserID 为空不参与匹配",
			list: []Account{
				{ID: "manual", UserID: "", ServiceToken: "old", Ph: "old", Active: true},
			},
			in:         Account{ID: "ext-1", UserID: "", ServiceToken: "new", Ph: "new", Active: true},
			wantAction: "added",
			wantID:     "ext-1",
			wantLen:    2,
		},
		{
			name: "收敛同 UserID 的历史重复行",
			list: []Account{
				{ID: "ext-1", UserID: "u1", ServiceToken: "t1", Ph: "p1", Active: true},
				{ID: "ext-2", UserID: "u1", ServiceToken: "t2", Ph: "p2", Active: true},
				{ID: "ext-3", UserID: "u2", ServiceToken: "t3", Ph: "p3", Active: true},
			},
			in:         Account{ID: "ext-9", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
			wantAction: "updated",
			wantID:     "ext-1",
			wantLen:    2,
			want: []Account{
				{ID: "ext-1", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
				{ID: "ext-3", UserID: "u2", ServiceToken: "t3", Ph: "p3", Active: true},
			},
		},
		{
			name: "按 ID 命中优先，并清掉与之同 UserID 的其他账号",
			list: []Account{
				{ID: "a", UserID: "u1", ServiceToken: "ta", Ph: "pa", Active: true},
				{ID: "b", UserID: "u2", ServiceToken: "tb", Ph: "pb", Active: true},
			},
			in:         Account{ID: "b", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
			wantAction: "updated",
			wantID:     "b",
			wantLen:    1,
			want: []Account{
				{ID: "b", UserID: "u1", ServiceToken: "new", Ph: "new", Active: true},
			},
		},
		{
			name: "覆盖时保持原有顺序",
			list: []Account{
				{ID: "a", UserID: "u1", ServiceToken: "ta", Ph: "pa", Active: true},
				{ID: "b", UserID: "u2", ServiceToken: "tb", Ph: "pb", Active: true},
				{ID: "c", UserID: "u3", ServiceToken: "tc", Ph: "pc", Active: true},
			},
			in:         Account{ID: "b", UserID: "u2", ServiceToken: "new", Ph: "new", Active: false},
			wantAction: "updated",
			wantID:     "b",
			wantLen:    3,
			want: []Account{
				{ID: "a", UserID: "u1", ServiceToken: "ta", Ph: "pa", Active: true},
				{ID: "b", UserID: "u2", ServiceToken: "new", Ph: "new", Active: false},
				{ID: "c", UserID: "u3", ServiceToken: "tc", Ph: "pc", Active: true},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, result := UpsertAccount(c.list, c.in)
			if result.Action != c.wantAction || result.ID != c.wantID {
				t.Fatalf("result = %+v, want action=%s id=%s", result, c.wantAction, c.wantID)
			}
			if len(got) != c.wantLen {
				t.Fatalf("len = %d, want %d (%+v)", len(got), c.wantLen, got)
			}
			if c.want == nil {
				return
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("got[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// 追加不得改写入参切片：Get 返回的是浅拷贝，底层数组与全局共享。
func TestUpsertAccountDoesNotMutateInput(t *testing.T) {
	list := []Account{{ID: "a", UserID: "u1", ServiceToken: "ta", Ph: "pa", Active: true}}
	ac := make([]Account, len(list))
	copy(ac, list)

	if _, result := UpsertAccount(list, Account{ID: "b", UserID: "u2", Active: true}); result.Action != "added" {
		t.Fatalf("action = %s, want added", result.Action)
	}
	for i := range ac {
		if list[i] != ac[i] {
			t.Fatalf("input mutated at %d: %+v", i, list[i])
		}
	}
}
