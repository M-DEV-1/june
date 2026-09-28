/** One of Ora's replies, rendered as text a person actually reads rather than as one plain paragraph: tables, task lists and strikethrough from GitHub-flavoured markdown, $...$ and $$...$$ as real typeset maths, headings no bigger than the page's own section heading, and a link that opens in the system browser instead of navigating this window away from the chat. No raw HTML ever runs — react-markdown's default turns a stray `<script>` or `<img onerror>` back into the literal text, which is also what keeps a reply safe from anything a tool result or a model slipped into the words. Input: the reply's text, exactly as the daemon sent it. Output: the structured reply. A user's own turn is left as plain text elsewhere — this is only for what Ora said back. */

import { useState, type ComponentPropsWithoutRef, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkMath from "remark-math";
import rehypeKatex from "rehype-katex";
import "katex/dist/katex.min.css";

import { useOpenUrlMutation } from "./api";
import { ui, useAppDispatch } from "./store";

/** Whether a link is one the daemon will actually open. Input: the href, already through react-markdown's own urlTransform, which blanks every scheme but http, https, irc, mailto and xmpp. Output: true for http and https, which is all POST /open takes (see internal/ipc/open.go, which refuses the rest before running any command). */
function opens(href: string | undefined): boolean {
  return /^https?:\/\//i.test(href ?? "");
}

/** What a link the daemon would refuse is drawn as instead of a link, said in the title so the address is not simply dead under the pointer. */
const NOT_OPENABLE = "Ora opens http and https links only, so this is shown as text.";

/** How long the copy button says what happened before going back to offering the copy. Long enough to read, short enough that the button is ready again by the time anyone reaches for it twice. */
const COPIED_MS = 1600;

/** A fenced code block with a way to get its text out. Ora writes prompts, commands and briefs into these for the user to use somewhere else, and selecting one by hand in a narrow column that scrolls sideways is a drag past the edge of the pane.
 * The text copied is the block's own textContent read off the DOM, not the markdown behind it: what the reader sees is what lands on the clipboard, with no fence and no language tag.
 * Input: the children react-markdown built for the block. Output: the block, with the button over its top-right corner.
 */
function CodeBlock({ children, ...p }: { children?: ReactNode }) {
  const [said, setSaid] = useState<"" | "Copied" | "Could not copy">("");
  const copy = (e: React.MouseEvent<HTMLButtonElement>) => {
    // The trailing newline a fenced block always ends with is dropped: pasted into a terminal it would run the command rather than leave it on the prompt to be read first.
    const text = (e.currentTarget.parentElement?.querySelector("pre")?.textContent ?? "").replace(/\n$/, "");
    // A WebKitGTK webview off a secure origin has no navigator.clipboard, so this is asked for rather than assumed, and a refusal is said on the button instead of thrown into the console.
    void Promise.resolve()
      .then(() => navigator.clipboard.writeText(text))
      .then(() => setSaid("Copied"))
      .catch(() => setSaid("Could not copy"))
      .finally(() => setTimeout(() => setSaid(""), COPIED_MS));
  };
  return (
    <div className="group relative mt-2">
      <pre className="overflow-x-auto rounded-md bg-sunken p-3 pr-11 font-mono text-meta" {...p}>
        {children}
      </pre>
      {/* Always there rather than on hover: a control that appears only under the pointer is one nobody finds, and the block has room for it. */}
      <button
        type="button"
        aria-label={said || "Copy"}
        title={said || "Copy"}
        onClick={copy}
        className="absolute top-1.5 right-1.5 flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-hover hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        {said === "Copied" ? <Check className="size-3.5 text-primary" /> : <Copy className="size-3.5" />}
      </button>
    </div>
  );
}

/** A link in a reply, opened in the system browser. A component of its own because it calls a hook: react-markdown renders a `components` entry through createElement, but an arrow function hung off an object is not a component to any tool that reads the code, and a hook inside one is a rules-of-hooks violation on its face. */
function ReplyLink({ href, children, ...p }: ComponentPropsWithoutRef<"a">) {
  const dispatch = useAppDispatch();
  const [openUrl] = useOpenUrlMutation();
  // A mailto:, a file: or a javascript: href react-markdown has already blanked is not something this window can do anything with — the daemon refuses it and the window never navigates itself — so it is drawn as the words it was, with the reason on the title, rather than as a click that silently does nothing.
  if (!opens(href))
    return (
      <span className="underline decoration-dotted underline-offset-2" title={NOT_OPENABLE}>
        {children}
      </span>
    );
  return (
    <a
      href={href}
      // text-primary, not text-accent: --accent in this window is rgba(0,0,0,.045), a hover tint meant for a background, so a link set in it was 4.5% black on a white card and all but invisible in the Sources list. --primary (#5a45ea) is the violet the ring and the focus states use, and reads at 6.5:1 on white.
      className="text-primary underline underline-offset-2"
      // Opened in the system browser through the daemon's POST /open (see internal/ipc/open.go) rather than window.open, which a Tauri WebKitGTK webview does not reliably hand off to the real browser; a click never touches window.location either. A refusal is said on the rail, since nothing else would show the click went nowhere.
      onClick={(e) => {
        e.preventDefault();
        if (href) openUrl(href).unwrap().catch(() => dispatch(ui.noticed({ text: "Could not open that link", kind: "error" })));
      }}
      {...p}
    >
      {children}
    </a>
  );
}

/** Every tag react-markdown may ask for, mapped onto the window's own type scale and surfaces rather than the browser's defaults — a heading tops out at `text-doc`, the same size a page's own section heading uses, because a reply is a paragraph in a thread and not a document of its own. Each mapping drops `node`: react-markdown 10 hands every custom component the mdast node it was rendered from, and React 19 writes an unknown lowercase prop straight onto the element, which left node="[object Object]" on every paragraph, heading, cell and link in every reply. */
const components: Components = {
  h1: ({ node: _n, ...p }) => <h2 className="mt-6 mb-2 text-doc text-foreground" {...p} />,
  h2: ({ node: _n, ...p }) => <h2 className="mt-6 mb-2 text-doc text-foreground" {...p} />,
  h3: ({ node: _n, ...p }) => <h3 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h4: ({ node: _n, ...p }) => <h4 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h5: ({ node: _n, ...p }) => <h5 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h6: ({ node: _n, ...p }) => <h6 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  p: ({ node: _n, ...p }) => <p className="mt-3 whitespace-pre-wrap leading-relaxed first:mt-0" {...p} />,
  ul: ({ node: _n, ...p }) => <ul className="mt-3 ml-5 list-disc space-y-1 first:mt-0" {...p} />,
  ol: ({ node: _n, ...p }) => <ol className="mt-3 ml-5 list-decimal space-y-1 first:mt-0" {...p} />,
  blockquote: ({ node: _n, ...p }) => <blockquote className="mt-3 border-l-2 border-hairline pl-3 text-muted-foreground italic first:mt-0" {...p} />,
  hr: ({ node: _n, ...p }) => <hr className="my-4 border-hairline" {...p} />,
  a: ({ node: _n, ...p }) => <ReplyLink {...p} />,
  pre: ({ node: _n, children, ...p }) => <CodeBlock {...p}>{children}</CodeBlock>,
  code: ({ node: _n, className, children, ...p }) => {
    // A fenced block's own <code> sits inside the <pre> above and only needs the mono face; a bare `code` span is inline text and gets the subtle surface and padding the design calls "sunken". remark tags a fenced block's code with `language-xxx` only when the fence names one — a fence with no language (rare in practice, since every real reply names one) falls back to the inline styling nested inside the pre's own background, which is a harmless doubling rather than a wrong render. ponytail: className-sniffing, not full inline/block tracking — fine while every real fence in the daemon's replies names a language.
    const block = /language-/.test(className ?? "");
    return (
      <code className={block ? `font-mono ${className}` : "rounded-xs bg-sunken px-1 py-0.5 font-mono text-meta"} {...p}>
        {children}
      </code>
    );
  },
  table: ({ node: _n, ...p }) => <table className="my-2 border-collapse text-read" {...p} />,
  th: ({ node: _n, ...p }) => <th className="border border-hairline px-2 py-1 text-left font-medium" {...p} />,
  td: ({ node: _n, ...p }) => <td className="border border-hairline px-2 py-1" {...p} />,
};

/** Ora's reply, structured. Input: the text. Output: the rendered markdown. */
export function ReplyMarkdown({ text }: { text: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm, remarkMath]} rehypePlugins={[rehypeKatex]} components={components}>
      {text}
    </ReactMarkdown>
  );
}
