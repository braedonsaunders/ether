// Ether's progressive External subtitle adapter for the pinned Jellyfin Web.
// Only tracks explicitly marked by the broker enter this path.
(() => {
    const players = new WeakMap();
    const tracks = new WeakMap();
    function stop(player, slot) {
        const slots = players.get(player);
        if (!slots) return;
        for (const [key, state] of slots) {
            if (slot !== undefined && key !== slot) continue;
            state.close();
            slots.delete(key);
        }
    }
    function start(player, video, stream, item, slot, url) {
        stop(player, slot);
        let slots = players.get(player);
        if (!slots) players.set(player, slots = new Map());
        let reusable = tracks.get(video);
        if (!reusable) tracks.set(video, reusable = new Map());
        let track = reusable.get(slot);
        if (!track) {
            track = video.addTextTrack('subtitles', '', stream.Language || 'und');
            reusable.set(slot, track);
        }
        track.mode = 'showing';
        let controller, timer, closed = false, generation = 0, requestedAt = 0;
        const offset = () => ((player._currentPlayOptions && player._currentPlayOptions.transcodingOffsetTicks) || 0) / 1e7;
        const clear = () => {
            for (const cue of Array.from(track.cues || [])) track.removeCue(cue);
        };
        const cancel = () => {
            generation++;
            if (controller) controller.abort();
            clearTimeout(timer);
        };
        const add = (data) => {
            if (data.Error) throw new Error(data.Error);
            for (const event of data.TrackEvents || []) {
                const begin = event.StartPositionTicks / 1e7 - offset();
                const end = event.EndPositionTicks / 1e7 - offset();
                if (end < video.currentTime - 30 || end <= begin) continue;
                const cue = new VTTCue(Math.max(0, begin), end, event.Text.replace(/\\N/g, '\n'));
                // JSON text comes from the server's plain ASS parser. Native
                // WebVTT interprets markup, so escape release-supplied text.
                cue.text = cue.text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
                track.addCue(cue);
            }
            for (const cue of Array.from(track.cues || [])) {
                if (cue.endTime < video.currentTime - 30) track.removeCue(cue);
            }
        };
        async function load() {
            cancel();
            clear();
            if (closed || video.ended || video.paused) return;
            const current = generation;
            controller = new AbortController();
            const target = new URL(url, document.baseURI);
            requestedAt = video.currentTime;
            target.searchParams.set('from_ms', Math.floor(Math.max(0, video.currentTime + offset() - 2) * 1000));
            try {
                const response = await fetch(target, { signal: controller.signal });
                if (!response.ok) throw new Error('subtitle request failed');
                if (!(response.headers.get('Content-Type') || '').includes('application/x-ndjson')) {
                    const data = await response.json();
                    if (!closed && current === generation) add(data);
                    return;
                }
                const reader = response.body.getReader();
                const decoder = new TextDecoder();
                let pending = '';
                for (;;) {
                    const { value, done } = await reader.read();
                    if (closed || current !== generation) return;
                    if (done) break;
                    pending += decoder.decode(value, { stream: true });
                    if (pending.length > 1048576) throw new Error('subtitle line too large');
                    let end;
                    while ((end = pending.indexOf('\n')) >= 0) {
                        const line = pending.slice(0, end);
                        pending = pending.slice(end + 1);
                        if (line.trim()) add(JSON.parse(line));
                    }
                }
            } catch (error) {
                if (closed || current !== generation || error.name === 'AbortError') return;
                controller.abort();
                // Retry from the current clock, never download the whole track.
                timer = setTimeout(load, 2000);
            }
        }
        const onPause = () => cancel();
        const onPlay = () => load();
        const onSeek = () => load();
        const onEnd = () => stop(player, slot);
        const onTime = () => { if (Math.abs(video.currentTime - requestedAt) > 60) load(); };
        const state = { close() {
            closed = true;
            cancel(); clear(); track.mode = 'disabled';
            video.removeEventListener('pause', onPause);
            video.removeEventListener('play', onPlay);
            video.removeEventListener('seeking', onSeek);
            video.removeEventListener('ended', onEnd);
            video.removeEventListener('emptied', onEnd);
            video.removeEventListener('timeupdate', onTime);
        } };
        slots.set(slot, state);
        video.addEventListener('pause', onPause);
        video.addEventListener('play', onPlay);
        video.addEventListener('seeking', onSeek);
        video.addEventListener('ended', onEnd);
        video.addEventListener('emptied', onEnd);
        video.addEventListener('timeupdate', onTime);
        player.setCueAppearance();
        load();
    }
    window.__etherSubtitles = { start, stop };
})();
