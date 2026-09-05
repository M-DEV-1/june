//! The daemon's event stream, read in Rust and handed to the overlay window.
//!
//! The obvious place to read the stream is the page itself, with EventSource or a fetch body, and that is what every other Ora client does. It does not work here. Measured on this machine on 2026-09-04: the daemon flushed a ring nine milliseconds after the POST that made it, and the overlay page's fetch reader had received zero bytes thirteen seconds later, then 124 bytes of a 230 byte event, then the whole event only once a second ring had been sent. WebKitGTK holds a small trickle of response body back in its network process, so an event that is the only thing on the wire waits for the next one. A three second ring is over before it is drawn.
//!
//! So the stream is read here instead, straight off a socket, and each event is handed to the overlay window as a Tauri event. The daemon speaks plain HTTP on 127.0.0.1, so this needs no HTTP client library: a GET, the response headers thrown away, and the chunked body decoded by hand.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::Duration;
use tauri::{AppHandle, Emitter};

/// Where the daemon listens. The same address every other Ora client uses.
const DAEMON_HOST: &str = "127.0.0.1";
const DAEMON_PORT: u16 = 6942;

/// The Tauri event the overlay page listens for. Its payload is the text of one event off the daemon's stream, exactly as the daemon wrote it.
pub const PAGE_EVENT: &str = "ora://daemon-event";

/// The window the events are handed to.
const OVERLAY_LABEL: &str = "overlay";

/// How long a dropped or refused connection waits before being dialled again.
const RECONNECT: Duration = Duration::from_secs(2);

/// How long to wait for the daemon to accept a connection before giving up and trying again.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

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

/// Reads the daemon's IPC token. Input: none; it reads <data dir>/ipc-token, the same file every Ora client reads. Output: the token with surrounding whitespace stripped, or None when the daemon has not written one yet.
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
        "GET /events HTTP/1.1\r\nHost: {address}\r\nX-Ora-Token: {token}\r\nAccept: text/event-stream\r\nConnection: keep-alive\r\n\r\n"
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
    eprintln!("ora: overlay event stream: connected");

    let mut buffer = String::new();
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
            // Some events ask the window itself to show something rather than asking the drawing layer to draw: that is how the daemon's tray opens the app window, now that this app has no tray of its own. Everything else, and every event that was acted on, still reaches the overlay page, which ignores what is not its business.
            crate::window_command(app, &payload);
            let _ = app.emit_to(OVERLAY_LABEL, PAGE_EVENT, payload);
        }
    }
}

/// Keeps a live connection to the daemon's event stream for as long as the app runs, on a thread of its own. Input: the app handle. Output: nothing; the thread never returns. A missing token, a refused connection or a dropped stream all end in the same wait and redial, so the layer survives the daemon starting later or restarting, and the token is read again on every dial in case the daemon minted a new one.
pub fn stream_events(app: AppHandle) {
    std::thread::spawn(move || loop {
        match read_token() {
            Some(token) => {
                if let Err(e) = read_stream(&app, &token) {
                    eprintln!("ora: overlay event stream: {e}");
                }
            }
            None => eprintln!("ora: overlay event stream: no ipc token yet"),
        }
        std::thread::sleep(RECONNECT);
    });
}

#[cfg(test)]
mod tests {
    use super::{read_chunk, sse_payloads};
    use std::io::BufReader;

    #[test]
    fn one_whole_event_is_read() {
        let mut buffer = String::from("data: {\"type\":\"overlay\"}\n\n");
        assert_eq!(
            sse_payloads(&mut buffer),
            vec!["{\"type\":\"overlay\"}".to_string()]
        );
        assert_eq!(buffer, "");
    }

    #[test]
    fn several_events_in_one_read_are_all_returned() {
        let mut buffer = String::from("data: one\n\ndata: two\n\n");
        assert_eq!(
            sse_payloads(&mut buffer),
            vec!["one".to_string(), "two".to_string()]
        );
    }

    #[test]
    fn a_half_arrived_event_is_kept_for_the_next_read() {
        let mut buffer = String::from("data: done\n\ndata: {\"half");
        assert_eq!(sse_payloads(&mut buffer), vec!["done".to_string()]);
        assert_eq!(buffer, "data: {\"half");
        buffer.push_str("\":1}\n\n");
        assert_eq!(sse_payloads(&mut buffer), vec!["{\"half\":1}".to_string()]);
    }

    #[test]
    fn nothing_is_returned_until_the_blank_line_arrives() {
        let mut buffer = String::from("data: still coming");
        assert!(sse_payloads(&mut buffer).is_empty());
        assert_eq!(buffer, "data: still coming");
    }

    #[test]
    fn the_data_lines_of_one_event_are_joined_with_newlines() {
        let mut buffer = String::from("data: first\ndata: second\n\n");
        assert_eq!(sse_payloads(&mut buffer), vec!["first\nsecond".to_string()]);
    }

    #[test]
    fn a_data_line_without_the_optional_space_is_read() {
        let mut buffer = String::from("data:tight\n\n");
        assert_eq!(sse_payloads(&mut buffer), vec!["tight".to_string()]);
    }

    #[test]
    fn a_keep_alive_comment_yields_no_event() {
        let mut buffer = String::from(": ping\n\ndata: real\n\n");
        assert_eq!(sse_payloads(&mut buffer), vec!["real".to_string()]);
    }

    #[test]
    fn chunks_are_read_one_at_a_time_until_the_zero_chunk() {
        let wire = b"5\r\nhello\r\n3\r\nbye\r\n0\r\n\r\n";
        let mut reader = BufReader::new(&wire[..]);
        assert_eq!(read_chunk(&mut reader).unwrap(), Some(b"hello".to_vec()));
        assert_eq!(read_chunk(&mut reader).unwrap(), Some(b"bye".to_vec()));
        assert_eq!(read_chunk(&mut reader).unwrap(), None);
    }

    #[test]
    fn a_chunk_size_carrying_an_extension_is_still_read() {
        let wire = b"5;name=value\r\nhello\r\n0\r\n\r\n";
        let mut reader = BufReader::new(&wire[..]);
        assert_eq!(read_chunk(&mut reader).unwrap(), Some(b"hello".to_vec()));
    }

    #[test]
    fn an_ended_connection_reads_as_no_chunk() {
        let wire = b"";
        let mut reader = BufReader::new(&wire[..]);
        assert_eq!(read_chunk(&mut reader).unwrap(), None);
    }
}
