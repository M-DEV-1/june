//! The daemon's event stream, read in Rust and handed to the overlay window.
//!
//! The obvious place to read the stream is the page itself, with EventSource or a fetch body, and that is what every other June client does. It does not work here. Measured on this machine on 2026-09-04: the daemon flushed a ring nine milliseconds after the POST that made it, and the overlay page's fetch reader had received zero bytes thirteen seconds later, then 124 bytes of a 230 byte event, then the whole event only once a second ring had been sent. WebKitGTK holds a small trickle of response body back in its network process, so an event that is the only thing on the wire waits for the next one. A three second ring is over before it is drawn.
//!
//! So the stream is read here instead, straight off a socket, and each event is handed to the overlay window as a Tauri event. The daemon speaks plain HTTP on 127.0.0.1, so this needs no HTTP client library: a GET, the response headers thrown away, and the chunked body decoded by hand.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::sync::mpsc;
use std::time::Duration;
use tauri::{AppHandle, Emitter};

/// Where the daemon listens. The same address every other June client uses.
const DAEMON_HOST: &str = "127.0.0.1";
const DAEMON_PORT: u16 = 6942;

/// The Tauri event the overlay page listens for. Its payload is the text of one event off the daemon's stream, exactly as the daemon wrote it.
pub const PAGE_EVENT: &str = "june://daemon-event";

/// The window the events are handed to.
const OVERLAY_LABEL: &str = "overlay";

/// How long a dropped or refused connection waits before being dialled again.
const RECONNECT: Duration = Duration::from_secs(2);

/// How long to wait for the daemon to accept a connection before giving up and trying again.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

/// The "type" field of one daemon event, for the log line. Input: the JSON text of the event. Output: the type, or "?" when the payload is not an object with a string type.
fn payload_kind(payload: &str) -> String {
    serde_json::from_str::<serde_json::Value>(payload)
        .ok()
        .and_then(|v| v.get("type").and_then(|t| t.as_str()).map(str::to_string))
        .unwrap_or_else(|| "?".to_string())
}

/// Whether one event off the stream is worth a line in the window's log. Input: the JSON text of the event. Output: false for a "level" event, true for everything else.
/// A live voice session sends up to twenty level events a second and every one of them only moves a waveform, so logging them means twenty JSON parses and twenty log lines a second on the very thread that must not fall behind. The test is a substring rather than a parse because that is all it has to be: the daemon writes its events with encoding/json, which puts no space after the colon.
fn worth_logging(payload: &str) -> bool {
    !payload.contains("\"type\":\"level\"")
}

/// How many payloads may be waiting for the window before the next one is dropped. Small on purpose: this queue exists to keep the socket being read, not to store a backlog, and a page that is this far behind has nothing useful left to draw from the events in the middle.
const HANDOVER_QUEUE: usize = 32;

/// Puts one payload on the handing queue without ever waiting for it. Input: the queue, the payload, and whether a drop has already been logged since the last payload that got through. Output: Ok once the payload is either queued or dropped, Err when the handing thread has gone.
/// A payload that finds the queue full is dropped rather than waited on, because waiting is what costs the connection: the daemon drops a client that has not read eight events (clientBufferSize in internal/ipc/ipc.go). One line is logged per run of drops rather than per drop, so a stalled main loop does not turn a flood of events into a flood of log lines.
fn push(tx: &mpsc::SyncSender<String>, payload: String, dropping: &mut bool) -> std::io::Result<()> {
    match tx.try_send(payload) {
        Ok(()) => {
            *dropping = false;
            Ok(())
        }
        Err(mpsc::TrySendError::Full(_)) => {
            if !*dropping {
                *dropping = true;
                eprintln!("june: overlay is behind; dropping events until it catches up");
            }
            Ok(())
        }
        Err(mpsc::TrySendError::Disconnected(_)) => {
            Err(std::io::Error::other("the overlay handing thread has gone"))
        }
    }
}

/// Hands payloads to the window on a thread of its own, so the socket is never held open by the handing over. Input: the app handle. Output: the sender to push each payload into; the thread ends when that sender is dropped.
/// The daemon drops a client that has not read eight events (clientBufferSize in internal/ipc/ipc.go), and window_command can hand work to the main loop while emit_to crosses into the webview, so doing either on the reading thread let a burst of shapes outrun the read and cost the whole connection. The reader now only pushes into this queue, and the queue is bounded so that a stalled main loop grows nothing (see push).
fn hand_over(app: AppHandle) -> mpsc::SyncSender<String> {
    let (tx, rx) = mpsc::sync_channel::<String>(HANDOVER_QUEUE);
    std::thread::spawn(move || {
        for payload in rx {
            crate::window_command(&app, &payload);
            if let Err(e) = app.emit_to(OVERLAY_LABEL, PAGE_EVENT, payload) {
                eprintln!("june: overlay could not hand an event to the page: {e}");
            }
        }
    });
    tx
}

/// Pulls the data payload of every complete server-sent event out of a buffer. Input: everything received and not yet parsed; on return it holds only the tail after the last blank line, which is the event still arriving. Output: one string per complete event, its "data:" lines joined by newlines.
/// An event carrying no data line at all — a lone comment, which is how a server keeps a connection warm — yields nothing. Both "data: x" and "data:x" are read, because the space after the colon is optional in the format.
fn sse_payloads(buffer: &mut String) -> Vec<String> {
    let Some(end) = buffer.rfind("\n\n") else {
        return Vec::new();
    };
    let complete = buffer[..end].to_string();
    let rest = buffer[end + 2..].to_string();
    *buffer = rest;

    let mut out = Vec::new();
    for block in complete.split("\n\n") {
        let mut lines = Vec::new();
        for line in block.split('\n') {
            let Some(value) = line.strip_prefix("data:") else {
                continue;
            };
            lines.push(value.strip_prefix(' ').unwrap_or(value).to_string());
        }
        if !lines.is_empty() {
            out.push(lines.join("\n"));
        }
    }
    out
}

/// Reads one chunk of an HTTP chunked-transfer body. Input: a reader sitting at the start of a chunk size line. Output: the chunk's bytes, or None at the final zero-sized chunk or at the end of the connection.
fn read_chunk<R: BufRead>(reader: &mut R) -> std::io::Result<Option<Vec<u8>>> {
    let mut line = String::new();
    if reader.read_line(&mut line)? == 0 {
        return Ok(None);
    }
    // A chunk size line is hexadecimal and may carry extensions after a semicolon, which nothing here needs.
    let head = line.trim();
    let digits = head.split(';').next().unwrap_or("");
    let Ok(size) = usize::from_str_radix(digits, 16) else {
        return Ok(None);
    };
    if size == 0 {
        return Ok(None);
    }
    let mut body = vec![0u8; size];
    reader.read_exact(&mut body)?;
    // Every chunk is followed by its own CRLF, which is not part of the data.
    let mut tail = [0u8; 2];
    reader.read_exact(&mut tail)?;
    Ok(Some(body))
}

/// Reads the daemon's IPC token. Input: none; it reads <data dir>/ipc-token, the same file every June client reads. Output: the token with surrounding whitespace stripped, or None when the daemon has not written one yet.
fn read_token() -> Option<String> {
    let dir = crate::data_dir()?;
    let text = std::fs::read_to_string(dir.join("ipc-token")).ok()?;
    let token = text.trim().to_string();
    if token.is_empty() {
        None
    } else {
        Some(token)
    }
}

/// Opens the daemon's /events stream and reads it until it ends, handing every event to the overlay window. Input: the app handle and the IPC token. Output: Ok when the stream closed cleanly, an error when it could not be opened or the connection broke; either way the caller dials again.
fn read_stream(app: &AppHandle, token: &str) -> std::io::Result<()> {
    let address = format!("{DAEMON_HOST}:{DAEMON_PORT}");
    let target = address
        .parse()
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "bad daemon address"))?;
    let mut socket = TcpStream::connect_timeout(&target, CONNECT_TIMEOUT)?;
    write!(
        socket,
        "GET /events HTTP/1.1\r\nHost: {address}\r\nX-June-Token: {token}\r\nAccept: text/event-stream\r\nConnection: keep-alive\r\n\r\n"
    )?;
    socket.flush()?;

    let mut reader = BufReader::new(socket);

    // The status line and the headers, read to the blank line that ends them. The only header that matters is whether the body is chunked, which it is whenever the daemon streams rather than sending a length.
    let mut status = String::new();
    reader.read_line(&mut status)?;
    if !status.contains(" 200 ") {
        return Err(std::io::Error::other(format!(
            "events answered {}",
            status.trim()
        )));
    }
    let mut chunked = false;
    loop {
        let mut header = String::new();
        if reader.read_line(&mut header)? == 0 {
            return Err(std::io::Error::other("connection closed in the headers"));
        }
        if header.trim().is_empty() {
            break;
        }
        if header
            .to_ascii_lowercase()
            .starts_with("transfer-encoding:")
            && header.to_ascii_lowercase().contains("chunked")
        {
            chunked = true;
        }
    }

    // One line per successful dial, so the window's log shows whether the drawing layer was ever subscribed when a drawing seems not to have appeared.
    eprintln!("june: overlay event stream: connected");

    let handing = hand_over(app.clone());

    let mut buffer = String::new();
    // Whether the queue was full the last time an event was pushed, so a run of drops is logged once (see push).
    let mut dropping = false;
    loop {
        let bytes = if chunked {
            match read_chunk(&mut reader)? {
                Some(bytes) => bytes,
                None => return Ok(()),
            }
        } else {
            let mut raw = [0u8; 4096];
            let n = reader.read(&mut raw)?;
            if n == 0 {
                return Ok(());
            }
            raw[..n].to_vec()
        };
        buffer.push_str(&String::from_utf8_lossy(&bytes));
        for payload in sse_payloads(&mut buffer) {
            // One line per event off the wire, bar the voice waveform's, so a drawing that never appeared can be placed: logged here and not drawn is the page's problem, never logged here is the daemon's or the connection's. The kind is only parsed out for an event that is going to be logged, so nothing on this thread parses a level tick.
            if worth_logging(&payload) {
                eprintln!(
                    "june: overlay event: kind={} bytes={}",
                    payload_kind(&payload),
                    payload.len()
                );
            }
            // Some events ask the window itself to show something rather than asking the drawing layer to draw: that is how the daemon's tray opens the app window, now that this app has no tray of its own. Everything else, and every event that was acted on, still reaches the overlay page, which ignores what is not its business. Both happen on the handing thread, so this loop goes straight back to reading the socket.
            push(&handing, payload, &mut dropping)?;
        }
    }
}

/// Keeps a live connection to the daemon's event stream for as long as the app runs, on a thread of its own. Input: the app handle. Output: nothing; the thread never returns. A missing token, a refused connection or a dropped stream all end in the same wait and redial, so the layer survives the daemon starting later or restarting, and the token is read again on every dial in case the daemon minted a new one.
pub fn stream_events(app: AppHandle) {
    std::thread::spawn(move || loop {
        match read_token() {
            Some(token) => {
                if let Err(e) = read_stream(&app, &token) {
                    eprintln!("june: overlay event stream: {e}");
                }
            }
            None => eprintln!("june: overlay event stream: no ipc token yet"),
        }
        std::thread::sleep(RECONNECT);
    });
}

#[cfg(test)]
mod tests {
    use super::{push, read_chunk, sse_payloads};
    use std::io::BufReader;
    use std::sync::mpsc;

    #[test]
    fn a_full_queue_drops_the_event_and_a_gone_reader_ends_the_read() {
        let (tx, rx) = mpsc::sync_channel::<String>(1);
        let mut dropping = false;
        push(&tx, "one".to_string(), &mut dropping).unwrap();
        push(&tx, "two".to_string(), &mut dropping).unwrap();
        // The queue took the first and not the one that found it full; nothing waited on the reader.
        assert!(dropping);
        assert_eq!(rx.recv().unwrap(), "one");
        assert!(rx.try_recv().is_err());
        // Room again, so the next event goes through and the next run of drops gets its own log line.
        push(&tx, "three".to_string(), &mut dropping).unwrap();
        assert!(!dropping);
        drop(rx);
        assert!(push(&tx, "four".to_string(), &mut dropping).is_err());
    }

    #[test]
    fn server_sent_events_are_read_as_they_arrive() {
        // A keep-alive comment yields nothing, the space after "data:" is optional, and the data lines of one event are joined with newlines.
        let mut buffer = String::from(": ping\n\ndata: one\n\ndata:first\ndata: second\n\ndata: {\"half");
        assert_eq!(
            sse_payloads(&mut buffer),
            vec!["one".to_string(), "first\nsecond".to_string()]
        );
        // A half-arrived event waits for its blank line.
        assert_eq!(buffer, "data: {\"half");
        buffer.push_str("\":1}\n\n");
        assert_eq!(sse_payloads(&mut buffer), vec!["{\"half\":1}".to_string()]);
        assert_eq!(buffer, "");
    }

    #[test]
    fn chunks_are_read_one_at_a_time_until_the_zero_chunk() {
        let wire = b"5;name=value\r\nhello\r\n3\r\nbye\r\n0\r\n\r\n";
        let mut reader = BufReader::new(&wire[..]);
        assert_eq!(read_chunk(&mut reader).unwrap(), Some(b"hello".to_vec()));
        assert_eq!(read_chunk(&mut reader).unwrap(), Some(b"bye".to_vec()));
        assert_eq!(read_chunk(&mut reader).unwrap(), None);
        // An ended connection reads as no chunk too.
        assert_eq!(read_chunk(&mut BufReader::new(&b""[..])).unwrap(), None);
    }
}
