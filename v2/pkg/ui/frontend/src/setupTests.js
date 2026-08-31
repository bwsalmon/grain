// Adds the `toBeInTheDocument`/`toHaveTextContent`/... matchers Testing
// Library tests below use, extending Vitest's own `expect` the same way
// jest-dom extends Jest's.
import "@testing-library/jest-dom/vitest";

import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Testing Library only registers this itself when it finds Jest's global
// afterEach; without `test.globals` this project leaves off (imports are
// explicit everywhere else here), each render would otherwise pile up
// in the same jsdom document instead of unmounting between tests.
afterEach(cleanup);

// App.jsx (bwsalmon/agents#548) reads and writes window.location as
// part of keeping the address bar in sync with the current sub-page.
// jsdom's window -- and so its location -- outlives any one test within
// a file, so without resetting this too, one test's navigation would
// otherwise leak into the next test's initial render.
afterEach(() => {
  window.history.replaceState(null, "", "/");
});
