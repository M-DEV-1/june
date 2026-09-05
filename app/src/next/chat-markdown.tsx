/** One of Ora's replies, rendered as text a person actually reads rather than as one plain paragraph: tables, task lists and strikethrough from GitHub-flavoured markdown, $...$ and $$...$$ as real typeset maths, headings no bigger than the page's own section heading, and a link that opens in the system browser instead of navigating this window away from the chat. No raw HTML ever runs — react-markdown's default turns a stray `<script>` or `<img onerror>` back into the literal text, which is also what keeps a reply safe from anything a tool result or a model slipped into the words. Input: the reply's text, exactly as the daemon sent it. Output: the structured reply. A user's own turn is left as plain text elsewhere — this is only for what Ora said back. */

import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkMath from "remark-math";
import rehypeKatex from "rehype-katex";
import "katex/dist/katex.min.css";

/** Opens a link in the system browser rather than this window. A chat pane is somewhere to read, not somewhere to navigate away from, so a click never touches window.location — the same reasoning that keeps every other outbound link in Ora off this webview. Input: the href clicked. Output: nothing. */
function openLink(href: string): void {
  window.open(href, "_blank", "noopener,noreferrer");
}

/** Every tag react-markdown may ask for, mapped onto the window's own type scale and surfaces rather than the browser's defaults — a heading tops out at `text-doc`, the same size a page's own section heading uses, because a reply is a paragraph in a thread and not a document of its own. */
const components: Components = {
  h1: (p) => <h2 className="mt-6 mb-2 text-doc text-foreground" {...p} />,
  h2: (p) => <h2 className="mt-6 mb-2 text-doc text-foreground" {...p} />,
  h3: (p) => <h3 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h4: (p) => <h4 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h5: (p) => <h5 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  h6: (p) => <h6 className="mt-4 mb-1 text-ui font-medium text-foreground" {...p} />,
  p: (p) => <p className="whitespace-pre-wrap" {...p} />,
  ul: (p) => <ul className="ml-5 list-disc [&>li]:mt-1" {...p} />,
  ol: (p) => <ol className="ml-5 list-decimal [&>li]:mt-1" {...p} />,
  a: ({ href, children, ...p }) => (
    <a
      href={href}
      className="text-accent underline underline-offset-2"
      onClick={(e) => {
        e.preventDefault();
        if (href) openLink(href);
      }}
      {...p}
    >
      {children}
    </a>
  ),
  pre: (p) => <pre className="mt-2 overflow-x-auto rounded-md bg-sunken p-3 font-mono text-meta" {...p} />,
  code: ({ className, children, ...p }) => {
    // A fenced block's own <code> sits inside the <pre> above and only needs the mono face; a bare `code` span is inline text and gets the subtle surface and padding the design calls "sunken". remark tags a fenced block's code with `language-xxx` only when the fence names one — a fence with no language (rare in practice, since every real reply names one) falls back to the inline styling nested inside the pre's own background, which is a harmless doubling rather than a wrong render. ponytail: className-sniffing, not full inline/block tracking — fine while every real fence in the daemon's replies names a language.
    const block = /language-/.test(className ?? "");
    return (
      <code className={block ? `font-mono ${className}` : "rounded-xs bg-sunken px-1 py-0.5 font-mono text-meta"} {...p}>
        {children}
      </code>
    );
  },
  table: (p) => <table className="my-2 border-collapse text-read" {...p} />,
  th: (p) => <th className="border border-hairline px-2 py-1 text-left font-medium" {...p} />,
  td: (p) => <td className="border border-hairline px-2 py-1" {...p} />,
};

/** Ora's reply, structured. Input: the text. Output: the rendered markdown. */
export function ReplyMarkdown({ text }: { text: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm, remarkMath]} rehypePlugins={[rehypeKatex]} components={components}>
      {text}
    </ReactMarkdown>
  );
}
