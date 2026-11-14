package webclient

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProgressiveSubtitleAdapter(t *testing.T) {
	// A fixture directory can exercise exact anchors against the verified pinned
	// Debian artifact without adding another 30 MB copy to the repository.
	fixtures := os.Getenv("JELLYFIN_WEB_FIXTURE")
	for name, source := range map[string]string{
		"index.html":              `<script src="main.jellyfin.bundle.js?build"></script><script src="runtime.bundle.js?build"></script>`,
		"runtime.bundle.js":       `s.miniCssF=function(){return "test"};`,
		"main.jellyfin.bundle.js": "E.SubtitleProfiles=[];",
		"htmlVideoPlayer-plugin.02b37b7cfd0e06696bd1.chunk.js": `function(e,t,n){return F.apply(this,arguments)} key:"destroyCustomTrack",value:function(e,t){`,
	} {
		if fixtures != "" {
			raw, err := os.ReadFile(filepath.Join(fixtures, name))
			if err != nil {
				t.Fatal(err)
			}
			source = string(raw)
		}
		adapted, err := AdaptSubtitles(name, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(adapted), "ether") {
			t.Fatal("adapter missing")
		}
		if _, err := AdaptSubtitles(name, []byte("upstream changed")); err == nil {
			t.Fatal("unsupported upstream accepted")
		}
		if fixtures != "" && name != "index.html" {
			cmd := exec.Command("node", "--check")
			cmd.Stdin = strings.NewReader(string(adapted))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("adapted bundle syntax: %v %s", err, output)
			}
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "-e", `global.window=global;global.document={baseURI:"http://ether/"};global.VTTCue=class {constructor(start,end,text){this.startTime=start;this.endTime=end;this.text=text}};`+subtitleAdapter+adapterLifecycleCheck)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("adapter lifecycle: %v %s", err, out)
	}
}

const adapterLifecycleCheck = `
// Minimal media surface: test async consumption and cancellation without a
// browser, timers, network, or a second copy of the player implementation.
async function check() {
 const assert = require('node:assert/strict');
 const listeners = new Map();
 const created = [];
 const video = { currentTime: 5, paused: false, ended: false,
  addTextTrack() { const t={cues:[],addCue(c){this.cues.push(c)},removeCue(c){this.cues.splice(this.cues.indexOf(c),1)}};created.push(t);return t },
  addEventListener(e,f){listeners.set(e,f)}, removeEventListener(e){listeners.delete(e)} };
 const player = { _currentPlayOptions: { transcodingOffsetTicks: 100000000 },setCueAppearance(){} };
 let requests=[];
 global.fetch = async (url, options) => {
  let reads = 0;
  requests.push({url,signal:options.signal});
  return {ok:true,headers:{get(){return 'application/x-ndjson'}},body:{getReader(){return {
   async read(){
    if(reads++===0)return {done:false,value:Buffer.from(JSON.stringify({TrackEvents:[{Text:'héllo\\Nworld',StartPositionTicks:150000000,EndPositionTicks:170000000}]})+'\n')};
    return new Promise(resolve => options.signal.addEventListener('abort',()=>resolve({done:true}),{once:true}));
   }
  }}}};
 };
 const tick = () => new Promise(resolve=>setImmediate(resolve));
 window.__etherSubtitles.start(player,video,{Language:'eng'},null,0,'http://ether/subtitles/2.js?ether_stream=1');
 await tick();
 assert.equal(created[0].cues.length,1);
 assert.equal(created[0].cues[0].startTime,5);
 assert.equal(created[0].cues[0].text,'héllo\nworld');
 assert.equal(requests[0].url.searchParams.get('from_ms'),'13000');
 video.currentTime=20;listeners.get('seeking')();await tick();
 assert.equal(requests[0].signal.aborted,true);
 assert.equal(requests[1].url.searchParams.get('from_ms'),'28000');
 window.__etherSubtitles.start(player,video,{Language:'eng'},null,0,'http://ether/subtitles/3.js?ether_stream=1');await tick();
 assert.equal(created.length,1,'track reuse');assert.equal(requests[1].signal.aborted,true);
 listeners.get('pause')();assert.equal(requests[2].signal.aborted,true);
 window.__etherSubtitles.stop(player);
 assert.equal(created[0].cues.length,0);assert.equal(listeners.size,0);
}
check().catch(error=>{console.error(error);process.exitCode=1});
`
