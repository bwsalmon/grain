import GrainMark from "./GrainMark.jsx";

// LoadingScreen fills the app shell for the one stretch nothing else can
// render yet: the initial /api/config round trip App.jsx makes before
// mount (bwsalmon/agents#555). Even the sidebar needs config's
// targetRepos/capabilities, so there is nothing more specific to show in
// the meantime -- just the mark, at the hero size GrainMark.jsx already
// has a denser/jittered rendering tier for (docs/brand.md's "hero,
// marketing, splash" tier) but that nothing had used until now.
const HERO_SIZE = 320;

export default function LoadingScreen() {
  return (
    <div className="loading-screen">
      <GrainMark size={HERO_SIZE} animated title="grain" />
    </div>
  );
}
