package silo

import (
	"strconv"
	"testing"
)

func TestRememberLiveCardEvictsInsteadOfWiping(t *testing.T) {
	i := &instance{}
	for n := 0; n < liveCardCap+10; n++ {
		i.rememberLiveCard("ch"+strconv.Itoa(n), liveChannelCardMeta{title: "t"})
	}
	if got := len(i.liveCards); got != liveCardCap {
		t.Fatalf("cache holds %d cards after overflow, want %d", got, liveCardCap)
	}
	if _, ok := i.rememberedLiveCard("ch" + strconv.Itoa(liveCardCap+9)); !ok {
		t.Fatal("newest card missing after overflow")
	}
	// Re-remembering a card already cached must not evict anything.
	for k := range i.liveCards {
		i.rememberLiveCard(k, liveChannelCardMeta{title: "u"})
		break
	}
	if got := len(i.liveCards); got != liveCardCap {
		t.Fatalf("cache holds %d cards after update, want %d", got, liveCardCap)
	}
}
