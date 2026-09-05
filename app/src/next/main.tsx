/** The entry point of next.html: builds the Redux store's starting state from what is stored on this machine, opens the daemon's event stream, and mounts the window. Nothing else in the page runs before this. */

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Provider } from "react-redux";

import App from "./App";
import { devToken } from "./api";
import "./index.css";
import { progress, settings, store, ui, type Place, type Theme } from "./store";
import { applyTheme, readTheme, storeTheme } from "./theme";
import { installMock } from "./mock";
import { rememberWindow } from "./window";

// A page opened with ?mock=1 answers itself instead of the daemon, so the window can be looked at in a plain browser tab with nothing running behind it. Installed before anything reads, and on any other URL it does nothing.
const mocked = installMock();

// A mock page, and a page served by the Vite dev server, may also say which screen to open on, which conversation to open, and which theme to draw in, which is what lets one headless browser photograph every screen without clicking anything. The dev server is the same origin that may hand a token over in its query string (see devToken in api.ts), and it is never where the packaged window is served from, so neither of these is read in the real window.
const asked = new URLSearchParams(mocked || devToken(location) !== undefined ? location.search : "");
const place = asked.get("place");
const theme = asked.get("theme");
const chat = asked.get("chat");

// The theme the user last picked, shared with the old window through one localStorage key, seeded into the store and stamped on the root element before the first paint so the page never flashes the wrong colours.
const chosen: Theme = theme === "light" || theme === "dark" ? theme : readTheme();
store.dispatch(settings.themePicked(chosen));
void applyTheme(chosen).then((resolved) => store.dispatch(settings.themeResolved(resolved)));

// Writes the choice back whenever it changes, so the next open of either window starts on the same theme.
let lastTheme: Theme = chosen;
store.subscribe(() => {
  const now = store.getState().settings.theme;
  if (now === lastTheme) return;
  lastTheme = now;
  storeTheme(now);
});

// The window comes back the size it was and where it was, and keeps recording where it goes; nothing happens on the first run, because there is nothing stored yet, and a mock page never places a window at all.
if (!mocked) void rememberWindow();

if (place) store.dispatch(ui.placeShown(place as Place));
if (chat) store.dispatch(ui.conversationOpened(chat));

// One SSE connection for the whole window; the middleware in store.ts owns it and feeds every message into the progress slice.
store.dispatch(progress.streamOpened());

const root = document.getElementById("next");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <Provider store={store}>
        <App />
      </Provider>
    </StrictMode>,
  );
}
