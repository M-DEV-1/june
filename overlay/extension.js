// Ora's on-screen accessories: a word beside the clock saying what Ora is doing, and a transparent layer over the screen for the rings and numbered marks the daemon asks for. Everything is driven by the daemon's /events stream; the extension itself decides nothing.

import Clutter from 'gi://Clutter';
import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import Soup from 'gi://Soup?version=3.0';
import St from 'gi://St';

import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

const DAEMON = 'http://127.0.0.1:6942';
const TOKEN_PATH = GLib.build_filenamev([GLib.get_home_dir(), '.local', 'share', 'ora', 'ipc-token']);
// How long "done" stays beside the clock after an ask finishes.
const DONE_MS = 4000;
// How long a dropped or refused /events connection waits before redialing.
const RECONNECT_MS = 3000;

// log writes one line to the journal carrying the extension's uuid, so `journalctl --user -b -g ora@local` finds it.
function log(message) {
    console.log(`ora@local: ${message}`);
}

// readToken reads the daemon's IPC token from disk. Output: the token with surrounding whitespace stripped, or null when the file is missing or unreadable (the daemon is not running yet).
function readToken() {
    try {
        const [ok, bytes] = GLib.file_get_contents(TOKEN_PATH);
        if (!ok)
            return null;
        const token = new TextDecoder().decode(bytes).trim();
        return token === '' ? null : token;
    } catch (e) {
        return null;
    }
}

// parseEventLine turns one line of the SSE stream into an event object. Input: a raw line. Output: the decoded object for a "data: {...}" line, or null for a blank line, a comment, or a payload that is not JSON.
export function parseEventLine(line) {
    if (!line.startsWith('data: '))
        return null;
    try {
        return JSON.parse(line.slice(6));
    } catch (e) {
        return null;
    }
}

// monitorFor finds the monitor a rectangle belongs to by its centre point. Input: a rect in global screen coordinates. Output: the monitor object, or null when the rect's centre falls on no monitor (a stale rect from a screen layout that has since changed).
export function monitorFor(rect) {
    const cx = rect.x + rect.w / 2;
    const cy = rect.y + rect.h / 2;
    for (const m of Main.layoutManager.monitors) {
        if (cx >= m.x && cx < m.x + m.width && cy >= m.y && cy < m.y + m.height)
            return m;
    }
    return null;
}

export default class OraExtension extends Extension {
    enable() {
        this._running = true;
        this._timers = new Set();
        this._doneTimer = 0;
        this._drawTimer = 0;
        this._retryTimer = 0;

        // The word beside the clock, empty until the daemon says otherwise.
        this._word = new St.Label({
            style_class: 'ora-word',
            text: '',
            y_align: Clutter.ActorAlign.CENTER,
        });
        Main.panel._centerBox.add_child(this._word);

        // The drawing layer: a plain actor over every monitor. affectsInputRegion false keeps every click going to the window underneath.
        this._layer = new Clutter.Actor({reactive: false});
        Main.layoutManager.addChrome(this._layer, {
            affectsInputRegion: false,
            affectsStruts: false,
            trackFullscreen: false,
        });
        this._fitLayer();
        this._monitorsChangedId = Main.layoutManager.connect('monitors-changed', () => this._fitLayer());

        this._session = new Soup.Session();
        this._cancel = new Gio.Cancellable();
        this._seedWord();
        this._connect();
        log('enabled');
    }

    disable() {
        this._running = false;
        for (const id of this._timers)
            GLib.source_remove(id);
        this._timers.clear();

        this._cancel.cancel();
        this._session.abort();
        this._session = null;
        this._input = null;

        if (this._monitorsChangedId) {
            Main.layoutManager.disconnect(this._monitorsChangedId);
            this._monitorsChangedId = 0;
        }
        Main.layoutManager.removeChrome(this._layer);
        this._layer.destroy();
        this._layer = null;

        this._word.destroy();
        this._word = null;
        log('disabled');
    }

    // addTimer runs fn once after ms milliseconds and forgets the timer afterwards, so disable() only has live timers to cancel. Output: the timer id.
    _addTimer(ms, fn) {
        const id = GLib.timeout_add(GLib.PRIORITY_DEFAULT, ms, () => {
            this._timers.delete(id);
            if (this._running)
                fn();
            return GLib.SOURCE_REMOVE;
        });
        this._timers.add(id);
        return id;
    }

    _clearTimer(id) {
        if (id && this._timers.delete(id))
            GLib.source_remove(id);
        return 0;
    }

    // fitLayer sizes the drawing layer to cover every monitor, so a rect anywhere on the desk can be drawn in global coordinates.
    _fitLayer() {
        let right = 0;
        let bottom = 0;
        for (const m of Main.layoutManager.monitors) {
            right = Math.max(right, m.x + m.width);
            bottom = Math.max(bottom, m.y + m.height);
        }
        this._layer.set_position(0, 0);
        this._layer.set_size(right, bottom);
    }

    // ---- the word by the clock ----

    // setWord puts one word beside the clock, or clears it when text is empty, and cancels any pending "done" fade.
    _setWord(text) {
        this._doneTimer = this._clearTimer(this._doneTimer);
        this._word.text = text;
        log(`word ${text === '' ? '(empty)' : text}`);
    }

    // seedWord asks the daemon what the voice session is doing right now, so a session already running when the extension starts is not shown as idle.
    _seedWord() {
        const token = readToken();
        if (!token)
            return;
        const msg = Soup.Message.new('GET', `${DAEMON}/voice/status?token=${encodeURIComponent(token)}`);
        this._session.send_and_read_async(msg, GLib.PRIORITY_DEFAULT, this._cancel, (session, res) => {
            if (!this._running)
                return;
            try {
                const bytes = session.send_and_read_finish(res);
                const status = JSON.parse(new TextDecoder().decode(bytes.get_data()));
                if (status.active && status.state !== 'idle')
                    this._setWord(status.state);
            } catch (e) {
                // No daemon, or a body that is not the status JSON: the word simply stays empty until an event arrives.
            }
        });
    }

    // ---- the ring and the pen ----

    // draw replaces whatever is on the layer with this request's shapes and schedules their removal. Input: the decoded overlay payload {kind, label, rects, ttl_ms}. A "clear" kind just erases.
    _draw(spec) {
        this._drawTimer = this._clearTimer(this._drawTimer);
        this._layer.destroy_all_children();
        if (!spec || spec.kind === 'clear' || !Array.isArray(spec.rects))
            return;

        let drawn = 0;
        spec.rects.forEach((rect, i) => {
            const monitor = monitorFor(rect);
            if (!monitor)
                return;
            if (spec.kind === 'ring')
                this._drawRing(rect, rect.label || spec.label || '', monitor);
            else if (spec.kind === 'marks')
                this._drawMark(rect, i + 1);
            drawn++;
        });

        const ttl = Math.max(1, spec.ttl_ms || 3000);
        this._drawTimer = this._addTimer(ttl, () => this._layer.destroy_all_children());
        log(`drew ${spec.kind} on ${drawn} of ${spec.rects.length} rects for ${ttl}ms`);
    }

    // drawRing outlines one rectangle and puts its label just above it, kept inside the rect's own monitor.
    _drawRing(rect, label, monitor) {
        this._layer.add_child(new St.Widget({
            style_class: 'ora-ring',
            x: rect.x,
            y: rect.y,
            width: rect.w,
            height: rect.h,
        }));
        if (label === '')
            return;
        const text = new St.Label({style_class: 'ora-ring-label', text: label});
        this._layer.add_child(text);
        text.set_position(rect.x, Math.max(monitor.y, rect.y - 26));
    }

    // drawMark puts a numbered red circle at the top left corner of one rectangle.
    _drawMark(rect, number) {
        const mark = new St.Label({style_class: 'ora-mark', text: String(number)});
        this._layer.add_child(mark);
        mark.set_position(rect.x, rect.y);
    }

    // ---- the event stream ----

    // onEvent acts on one decoded event: voice state changes and ask progress become the word beside the clock, overlay events become drawings.
    _onEvent(ev) {
        switch (ev.type) {
        case 'state':
            this._setWord(ev.text === 'idle' ? '' : ev.text);
            break;
        case 'status':
            this._setWord('working');
            break;
        case 'done':
            this._setWord('done');
            this._doneTimer = this._addTimer(DONE_MS, () => this._setWord(''));
            break;
        case 'error':
            this._setWord('');
            break;
        case 'overlay':
            try {
                this._draw(JSON.parse(ev.text));
            } catch (e) {
                log(`overlay event was not JSON: ${e}`);
            }
            break;
        }
    }

    // connect opens the daemon's SSE stream. A missing token, a refused connection or a non-200 answer all end in a retry, so the extension survives the daemon starting later or restarting.
    _connect() {
        if (!this._running)
            return;
        const token = readToken();
        if (!token) {
            this._retry('no token file yet');
            return;
        }
        const msg = Soup.Message.new('GET', `${DAEMON}/events?token=${encodeURIComponent(token)}`);
        this._session.send_async(msg, GLib.PRIORITY_DEFAULT, this._cancel, (session, res) => {
            if (!this._running)
                return;
            let stream;
            try {
                stream = session.send_finish(res);
            } catch (e) {
                this._retry(`connect failed: ${e}`);
                return;
            }
            if (msg.get_status() !== Soup.Status.OK) {
                this._retry(`events returned ${msg.get_status()}`);
                return;
            }
            log('connected to /events');
            this._input = new Gio.DataInputStream({base_stream: stream});
            this._readLine();
        });
    }

    // readLine reads one line of the stream, acts on it, and queues the next read. A null line means the daemon closed the stream, which is a retry.
    _readLine() {
        this._input.read_line_async(GLib.PRIORITY_DEFAULT, this._cancel, (input, res) => {
            if (!this._running)
                return;
            let line;
            try {
                [line] = input.read_line_finish_utf8(res);
            } catch (e) {
                this._retry(`read failed: ${e}`);
                return;
            }
            if (line === null) {
                this._retry('stream closed');
                return;
            }
            const ev = parseEventLine(line);
            if (ev)
                this._onEvent(ev);
            this._readLine();
        });
    }

    // retry drops the current stream and dials again after RECONNECT_MS.
    _retry(why) {
        if (!this._running || this._retryTimer)
            return;
        this._input = null;
        log(`reconnecting in ${RECONNECT_MS}ms: ${why}`);
        this._retryTimer = this._addTimer(RECONNECT_MS, () => {
            this._retryTimer = 0;
            this._connect();
        });
    }
}
