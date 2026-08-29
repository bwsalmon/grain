// Shared overlay chrome: a backdrop that closes on a direct click (not
// one that bubbled up from the panel itself) plus the panel's own close
// button, both routed through the same onClose.
export default function Overlay({ onClose, className = "", children }) {
  return (
    <div className="overlay" onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <div className={`panel ${className}`.trim()}>
        <button className="close" onClick={onClose}>&times;</button>
        {children}
      </div>
    </div>
  );
}
