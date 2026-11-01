package admin

import "testing"

func TestShouldAdoptCacheVolume(t *testing.T) {
	if !ShouldAdoptCacheVolume(false, "", "/config/cache", "/config", true) {
		t.Fatal("fresh install with a mounted /cache should adopt it")
	}
	if ShouldAdoptCacheVolume(true, "", "/config/cache", "/config", true) {
		t.Fatal("a finished install keeps its saved folder")
	}
	if ShouldAdoptCacheVolume(false, "shared", "/config/cache", "/config", true) {
		t.Fatal("shared storage is not the docker volume")
	}
	if ShouldAdoptCacheVolume(false, "", "/config/cache", "/config", false) {
		t.Fatal("an unmounted /cache path is not adopted")
	}
	if ShouldAdoptCacheVolume(false, "", "/library/cache", "/config", true) {
		t.Fatal("a chosen folder is left alone")
	}
}
