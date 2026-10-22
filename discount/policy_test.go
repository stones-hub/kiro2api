package discount

import (
	"strings"
	"testing"
	"time"

	"kiro2api/types"
)

func TestPolicyCapsOnlySuccessfulRepeatedPrefixes(t *testing.T) {
	c, err := NewWithPolicy(time.Minute, 100, 1024, Policy{MaxReadPercent: 40, MinUncachedTokens: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, repetitions := range []int{100, 19000, 26000, 50000} {
		req := &types.CountTokensRequest{Model: "claude-opus-4.6", Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: strings.Repeat("blue notebook on the shelf. ", repetitions)},
		}}
		first, err := c.Prepare("scope", req)
		if err != nil {
			t.Fatal(err)
		}
		if first.ReadTokens != 0 {
			t.Fatal("first request received a discount")
		}
		uncommitted, err := c.Prepare("scope", req)
		if err != nil {
			t.Fatal(err)
		}
		if uncommitted.ReadTokens != 0 {
			t.Fatal("pending request warmed cache")
		}
		first.Commit()
		repeated, err := c.Prepare("scope", req)
		if err != nil {
			t.Fatal(err)
		}
		if repeated.InputTokens != first.InputTokens {
			t.Fatal("policy changed total input")
		}
		if repeated.InputTokens <= 100000 {
			if repeated.ReadTokens != 0 {
				t.Fatal("short input received a discount")
			}
		} else {
			if repeated.ReadTokens <= 0 {
				t.Fatal("long repeated prefix received no discount")
			}
			if repeated.InputTokens-repeated.ReadTokens < 100000 {
				t.Fatal("ordinary input floor violated")
			}
			if int64(repeated.ReadTokens)*100 > int64(repeated.InputTokens)*40 {
				t.Fatal("40 percent ceiling violated")
			}
			if repeated.InputTokens > 200000 && repeated.ReadTokens != repeated.InputTokens*40/100 {
				t.Fatal("eligible prefix did not reach 40 percent cap")
			}
		}
		other, err := c.Prepare("another-user", req)
		if err != nil {
			t.Fatal(err)
		}
		if other.ReadTokens != 0 {
			t.Fatal("policy leaked a discount across user scopes")
		}
	}
}

func TestPolicyRejectsInvalidLimits(t *testing.T) {
	for _, policy := range []Policy{{MaxReadPercent: -1}, {MaxReadPercent: 101}, {MaxReadPercent: 40, MinUncachedTokens: -1}} {
		if _, err := NewWithPolicy(time.Minute, 10, 1024, policy); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestFixedDiscountOnHitsHasNoInputSizeFloor(t *testing.T) {
	for _, repetitions := range []int{1, 100, 19000, 50000} {
		c, err := NewWithPolicy(time.Minute, 100, 1, Policy{MaxReadPercent: 40, FixedPercentOnHit: true})
		if err != nil {
			t.Fatal(err)
		}
		req := &types.CountTokensRequest{Model: "claude-opus-4.6", Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: strings.Repeat("blue notebook on the shelf. ", repetitions)},
		}}
		first := prepare(t, c, "a", req)
		if first.ReadTokens != 0 || prepare(t, c, "a", req).ReadTokens != 0 {
			t.Fatal("first or pending request received a discount")
		}
		first.Commit()
		repeat := prepare(t, c, "a", req)
		if repeat.InputTokens != first.InputTokens || repeat.ReadTokens != repeat.InputTokens*40/100 || repeat.ReadTokens == 0 {
			t.Fatalf("fixed allowance missing for input %d: cached %d", repeat.InputTokens, repeat.ReadTokens)
		}
		if prepare(t, c, "other-user", req).ReadTokens != 0 {
			t.Fatal("discount leaked across users")
		}
		c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
		if prepare(t, c, "a", req).ReadTokens != 0 {
			t.Fatal("expired hit received a discount")
		}
	}
}

func TestFixedDiscountUsesFullInputAfterPartialPrefixHit(t *testing.T) {
	c, err := NewWithPolicy(time.Minute, 100, 1, Policy{MaxReadPercent: 40, FixedPercentOnHit: true})
	if err != nil {
		t.Fatal(err)
	}
	req := &types.CountTokensRequest{Model: "claude-opus-4.6",
		System:   []types.AnthropicSystemMessage{{Type: "text", Text: "Stable short reference."}},
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "First question"}},
	}
	first := prepare(t, c, "a", req)
	first.Commit()
	req.Messages[0].Content = strings.Repeat("Different new question. ", 300)
	repeat := prepare(t, c, "a", req)
	if repeat.ReadTokens != repeat.InputTokens*40/100 || repeat.ReadTokens <= first.InputTokens {
		t.Fatal("partial hit did not grant the fixed full-input allowance")
	}
	req.System[0].Text = "Unseen reference."
	if prepare(t, c, "a", req).ReadTokens != 0 {
		t.Fatal("an unmatched prefix received a discount")
	}
}
