/**
 * grain-mark.js — Grain brand mark renderer
 *
 * Vendored from the grain-mark design pack. Kept as close to the pack's
 * own copy as possible so a later revision of the mark can be dropped in
 * with a diff rather than a port; the deviations from it are:
 *
 *   - `frameWidth` (below) is an option instead of a per-style constant,
 *     so the animated mark and the exported static one can be told to
 *     draw the same ring. Left unset, both draw exactly the widths the
 *     pack hard-coded (0.05 of the width for `filled`, 0.012 otherwise),
 *     which at grain's icon sizes differ enough that switching between
 *     them reads as the ring changing rather than the figure animating.
 *
 * See docs/brand.md for what the mark means and where grain uses it.
 *
 * A Chladni-plate resonance figure inside a circular frame. Grains (particles)
 * sit evenly along the nodal lines of a square-plate eigenmode and snap between
 * modes with a mid-flight scatter. Below a size threshold the same figure is
 * rendered as strokes or filled regions, which survive at icon scale.
 *
 * Works in any browser canvas. No dependencies. ES module.
 *
 * Usage:
 *   import { createGrainMark, renderStatic, MODES, THEMES } from './grain-mark.js';
 *
 *   const mark = createGrainMark(canvas, { theme: 'light' });
 *   mark.start();                 // auto-cycle through MODES
 *   mark.setMode(2);              // jump to a mode (0-based index into MODES) — grains fly there
 *   mark.stop();
 *
 *   renderStatic(canvas, { mode: [2,3,-1], theme: 'dark', style: 'grains' });
 *   renderStatic(iconCanvas, { mode: [2,3,-1], style: 'filled' });  // for 24px tier
 */

// ---------- tuned constants (from the design session) ----------
export const DEFAULTS = {
  grainCount:   2000,    // hero grains at ~440px
  grainRadius:  0.0065,  // fraction of canvas width
  snapSeconds:  0.75,    // grain flight time between modes
  dwellSeconds: 0.33,    // hold time on each mode
  scatter:      0.07,    // fraction of frame radius grains stray mid-flight
  jitter:       0.9,     // px of perpendicular scatter along lines (hero only)
  zoom:         1.0,     // figure scale relative to frame (>1 enlarges)
  shape:        'circle',// 'circle' | 'hex'
  theme:        'dark',
  frameWidth:   null,    // ring width as a fraction of canvas width; null = per-style default
};

// [n, m, sign]  sign -1 = antisymmetric (has diagonals), +1 = symmetric
export const MODES = [[1,4,-1],[2,3,1],[2,3,-1],[1,4,1]];

export const THEMES = {
  dark:  { grain:'#D9A441', grainRGB:[0xD9,0xA4,0x41], border:'rgba(217,164,65,0.9)' },
  light: { grain:'#8A6A2E', grainRGB:[0x8A,0x6A,0x2E], border:'rgba(138,106,46,0.95)' },
};

// Size tiers: how to render at a given canvas pixel size.
// Grains for ≥48px (with tier-specific density), filled regions at 24px.
export const TIERS = [
  { minPx: 300, style:'grains', count: 2000, radius: 0.0065, jitter: 0.9 },
  { minPx: 80,  style:'grains', count: 520,  radius: 0.013,  jitter: 0   },
  { minPx: 40,  style:'grains', count: 520,  radius: 0.017,  jitter: 0   },
  { minPx: 0,   style:'filled' },
];
export function tierFor(px){ return TIERS.find(t => px >= t.minPx); }

// ---------- math ----------
export function fieldVal(nx, ny, n, m, sign){
  const px = nx*Math.PI, py = ny*Math.PI;
  return Math.cos(n*px)*Math.cos(m*py) + sign*Math.cos(m*px)*Math.cos(n*py);
}

const SEG = {
  0:[], 1:[[3,0]], 2:[[0,1]], 3:[[3,1]],
  4:[[1,2]], 5:[[3,0],[1,2]], 6:[[0,2]], 7:[[3,2]],
  8:[[2,3]], 9:[[0,2]], 10:[[0,1],[2,3]], 11:[[1,2]],
  12:[[3,1]], 13:[[0,1]], 14:[[3,0]], 15:[]
};

export function makeFrame(cx, cy, Rframe, shape){
  const verts = [];
  for (let i=0;i<6;i++){
    const a = -Math.PI/2 + i*Math.PI/3;
    verts.push({x:cx+Math.cos(a)*Rframe, y:cy+Math.sin(a)*Rframe});
  }
  return {
    cx, cy, R: Rframe, shape, verts,
    path(ctx){
      ctx.beginPath();
      if (shape==='circle') ctx.arc(cx,cy,Rframe,0,Math.PI*2);
      else verts.forEach((p,i)=> i===0?ctx.moveTo(p.x,p.y):ctx.lineTo(p.x,p.y));
      ctx.closePath();
    },
    contains(x,y){
      if (shape==='circle'){ const dx=x-cx, dy=y-cy; return dx*dx+dy*dy <= Rframe*Rframe; }
      let sign=null;
      for (let i=0;i<6;i++){
        const a=verts[i], b=verts[(i+1)%6];
        const c=(b.x-a.x)*(y-a.y)-(b.y-a.y)*(x-a.x);
        const s=c>0;
        if (sign===null) sign=s; else if (s!==sign) return false;
      }
      return true;
    },
  };
}

/** Trace the nodal figure (marching squares), prune edge bumps, return segments. */
export function traceFigure(frame, Rfield, mode, res){
  const [n,m,sign] = mode;
  const span = frame.R*1.05;
  const step = span*2/res;
  const sx = frame.cx-span, sy = frame.cy-span;
  const vals=[];
  for (let j=0;j<=res;j++){
    const row=[];
    for (let i=0;i<=res;i++){
      const x=sx+i*step, y=sy+j*step;
      row.push(fieldVal((x-frame.cx)/Rfield,(y-frame.cy)/Rfield,n,m,sign));
    }
    vals.push(row);
  }
  let segs=[];
  for (let j=0;j<res;j++){
    for (let i=0;i<res;i++){
      const x0=sx+i*step, y0=sy+j*step;
      const vTL=vals[j][i], vTR=vals[j][i+1], vBR=vals[j+1][i+1], vBL=vals[j+1][i];
      let c=0;
      if (vTL>=0) c|=1; if (vTR>=0) c|=2; if (vBR>=0) c|=4; if (vBL>=0) c|=8;
      const list=SEG[c];
      if (!list.length) continue;
      const ep=(idx)=>{
        if (idx===0){ const t=vTL/(vTL-vTR); return {x:x0+t*step,y:y0}; }
        if (idx===1){ const t=vTR/(vTR-vBR); return {x:x0+step,y:y0+t*step}; }
        if (idx===2){ const t=vBR/(vBR-vBL); return {x:x0+step-t*step,y:y0+step}; }
        const t=vBL/(vBL-vTL); return {x:x0,y:y0+step-t*step};
      };
      for (const [a,b] of list){
        const p1=ep(a), p2=ep(b);
        if (!frame.contains((p1.x+p2.x)/2,(p1.y+p2.y)/2)) continue;
        const len=Math.hypot(p2.x-p1.x,p2.y-p1.y);
        if (len<1e-6) continue;
        segs.push({p1,p2,len});
      }
    }
  }
  // prune short chains that hug the frame edge without reaching inward
  const key=(p)=>Math.round(p.x*4)+','+Math.round(p.y*4);
  const adj=new Map();
  segs.forEach((sg,i)=>{ for (const k of [key(sg.p1),key(sg.p2)]){ if(!adj.has(k)) adj.set(k,[]); adj.get(k).push(i);} });
  const seen=new Array(segs.length).fill(false);
  const keep=[];
  for (let i=0;i<segs.length;i++){
    if (seen[i]) continue;
    const comp=[]; const stack=[i]; seen[i]=true;
    while (stack.length){
      const c=stack.pop(); comp.push(c);
      for (const k of [key(segs[c].p1),key(segs[c].p2)]) for (const j of adj.get(k)) if(!seen[j]){seen[j]=true;stack.push(j);}
    }
    let len=0, maxInward=0;
    for (const c of comp){
      len+=segs[c].len;
      for (const q of [segs[c].p1,segs[c].p2]){
        const d=frame.R-Math.hypot(q.x-frame.cx,q.y-frame.cy);
        if (d>maxInward) maxInward=d;
      }
    }
    const isBump = maxInward < frame.R*0.16 && len < frame.R*1.2;
    if (!isBump) comp.forEach(c=>keep.push(segs[c]));
  }
  return keep;
}

/** Evenly spaced points along the figure, with optional perpendicular jitter. */
export function figurePoints(segs, N, jitter){
  const pts=[];
  if (!segs.length) return pts;
  const total=segs.reduce((a,b)=>a+b.len,0);
  const spacing=total/N;
  let carry=spacing*0.5;
  for (const s of segs){
    const dx=s.p2.x-s.p1.x, dy=s.p2.y-s.p1.y;
    const nx=-dy/s.len, ny=dx/s.len;
    let d=carry;
    while (d<=s.len && pts.length<N){
      const f=d/s.len, jit=(Math.random()*2-1)*(jitter||0);
      pts.push({x:s.p1.x+dx*f+nx*jit, y:s.p1.y+dy*f+ny*jit});
      d+=spacing;
    }
    carry=d-s.len;
  }
  while (pts.length<N){
    const s=segs[Math.floor(Math.random()*segs.length)], f=Math.random();
    pts.push({x:s.p1.x+(s.p2.x-s.p1.x)*f, y:s.p1.y+(s.p2.y-s.p1.y)*f});
  }
  return pts;
}

// ---------- renderers ----------
function drawFrame(ctx, frame, W, theme, widthFrac){
  ctx.strokeStyle=theme.border;
  ctx.lineWidth=Math.max(1,W*widthFrac);
  frame.path(ctx); ctx.stroke();
}

/** Static render. style: 'grains' | 'strokes' | 'filled' (auto by size if omitted). */
export function renderStatic(canvas, opts={}){
  const o={...DEFAULTS,...opts};
  const ctx=canvas.getContext('2d');
  const W=canvas.width, H=canvas.height;
  const cx=W/2, cy=H/2, Rframe=Math.min(W,H)*0.44, Rfield=Rframe*o.zoom;
  const frame=makeFrame(cx,cy,Rframe,o.shape);
  const theme=THEMES[o.theme]||THEMES.dark;
  const tier=tierFor(W);
  const style=o.style||tier.style;
  const mode=o.mode||MODES[0];
  ctx.clearRect(0,0,W,H);

  if (style==='filled'){
    const SS=3, img=ctx.createImageData(W,H), d=img.data, rgb=theme.grainRGB;
    for (let y=0;y<H;y++) for (let x=0;x<W;x++){
      let cover=0;
      for (let sy=0;sy<SS;sy++) for (let sx=0;sx<SS;sx++){
        const px=x+(sx+0.5)/SS, py=y+(sy+0.5)/SS;
        if (!frame.contains(px,py)) continue;
        if (fieldVal((px-cx)/Rfield,(py-cy)/Rfield,mode[0],mode[1],mode[2])>0) cover++;
      }
      const i=(y*W+x)*4;
      d[i]=rgb[0]; d[i+1]=rgb[1]; d[i+2]=rgb[2]; d[i+3]=Math.round(cover/(SS*SS)*255);
    }
    ctx.putImageData(img,0,0);
    drawFrame(ctx,frame,W,theme,o.frameWidth ?? 0.05);
    return;
  }

  const segs=traceFigure(frame,Rfield,mode, style==='strokes'?48:64);
  ctx.save(); frame.path(ctx); ctx.clip();
  if (style==='strokes'){
    ctx.strokeStyle=theme.grain; ctx.lineWidth=Math.max(0.75,W*0.028);
    ctx.lineCap='round'; ctx.lineJoin='round';
    for (const s of segs){ ctx.beginPath(); ctx.moveTo(s.p1.x,s.p1.y); ctx.lineTo(s.p2.x,s.p2.y); ctx.stroke(); }
  } else {
    const count=o.grainCount ?? tier.count, radius=o.grainRadius ?? tier.radius, jitter=o.jitter ?? tier.jitter;
    const pts=figurePoints(segs,count,jitter);
    ctx.fillStyle=theme.grain;
    const r=Math.max(0.6,W*radius);
    for (const p of pts){ ctx.beginPath(); ctx.arc(p.x,p.y,r,0,Math.PI*2); ctx.fill(); }
  }
  ctx.restore();
  drawFrame(ctx,frame,W,theme,o.frameWidth ?? 0.012);
}

/** Animated mark. Returns controller { start, stop, setMode, next, setTheme, destroy }. */
export function createGrainMark(canvas, opts={}){
  const o={...DEFAULTS,...opts};
  const ctx=canvas.getContext('2d');
  const W=canvas.width, H=canvas.height;
  const cx=W/2, cy=H/2, Rframe=Math.min(W,H)*0.44, Rfield=Rframe*o.zoom;
  const frame=makeFrame(cx,cy,Rframe,o.shape);
  const tier=tierFor(W);
  const count=o.grainCount ?? tier.count, radius=o.grainRadius ?? tier.radius, jitter=o.jitter ?? tier.jitter;
  const modes=o.modes||MODES;
  let theme=THEMES[o.theme]||THEMES.dark;

  const grains=[];
  for (let i=0;i<count;i++) grains.push({x:cx,y:cy,sx:cx,sy:cy,tx:cx,ty:cy,kx:0,ky:0});

  function assignTargets(mode){
    const targets=figurePoints(traceFigure(frame,Rfield,mode,64),count,jitter);
    // greedy nearest matching with spatial buckets
    const cell=Math.max(4,Rframe*0.08), cols=Math.ceil(W/cell), rows=Math.ceil(H/cell);
    const buckets=Array.from({length:cols*rows},()=>[]);
    grains.forEach((g,i)=>{
      const bx=Math.min(cols-1,Math.max(0,Math.floor(g.x/cell))), by=Math.min(rows-1,Math.max(0,Math.floor(g.y/cell)));
      buckets[by*cols+bx].push(i);
    });
    for (const t of targets.slice().sort(()=>Math.random()-0.5)){
      const bx=Math.min(cols-1,Math.max(0,Math.floor(t.x/cell))), by=Math.min(rows-1,Math.max(0,Math.floor(t.y/cell)));
      let best=-1,bd=Infinity,bb=null,bp=-1;
      for (let ring=0; ring<Math.max(cols,rows); ring++){
        for (let yy=by-ring; yy<=by+ring; yy++){ if (yy<0||yy>=rows) continue;
          for (let xx=bx-ring; xx<=bx+ring; xx++){ if (xx<0||xx>=cols) continue;
            if (Math.abs(yy-by)!==ring && Math.abs(xx-bx)!==ring) continue;
            const b=buckets[yy*cols+xx];
            for (let q=0;q<b.length;q++){ const g=grains[b[q]]; const d=(g.x-t.x)**2+(g.y-t.y)**2; if (d<bd){bd=d;best=b[q];bb=b;bp=q;} }
          } }
        if (best>=0 && Math.sqrt(bd) < ring*cell) break;
      }
      if (best<0) break;
      bb.splice(bp,1);
      const g=grains[best];
      g.sx=g.x; g.sy=g.y; g.tx=t.x; g.ty=t.y;
      const a=Math.random()*Math.PI*2, mag=(0.3+Math.random()*0.7)*Rframe*o.scatter;
      g.kx=Math.cos(a)*mag; g.ky=Math.sin(a)*mag;
    }
  }
  function place(k){
    const bulge=Math.sin(Math.PI*k);
    for (const g of grains){ g.x=g.sx+(g.tx-g.sx)*k+g.kx*bulge; g.y=g.sy+(g.ty-g.sy)*k+g.ky*bulge; }
  }
  function draw(){
    ctx.clearRect(0,0,W,H);
    ctx.save(); frame.path(ctx); ctx.clip();
    ctx.fillStyle=theme.grain;
    const r=Math.max(0.6,W*radius);
    for (const g of grains){ ctx.beginPath(); ctx.arc(g.x,g.y,r,0,Math.PI*2); ctx.fill(); }
    ctx.restore();
    drawFrame(ctx,frame,W,theme,o.frameWidth ?? 0.012);
  }

  let idx=0, snapT=1, dwellT=0, auto=false, raf=null, last=0;
  function setMode(i){ idx=(i+modes.length)%modes.length; assignTargets(modes[idx]); snapT=0; dwellT=0; if (!raf) loop(); }
  function loop(){
    last=performance.now();
    const frameFn=(now)=>{
      const dt=Math.min(0.05,(now-last)/1000); last=now;
      if (snapT<1){
        snapT=Math.min(1,snapT+dt/o.snapSeconds);
        const e=snapT<0.5 ? 4*snapT**3 : 1-Math.pow(-2*snapT+2,3)/2;
        place(e);
      } else if (auto){
        dwellT+=dt; if (dwellT>=o.dwellSeconds) setMode(idx+1);
      }
      draw();
      if (auto || snapT<1) raf=requestAnimationFrame(frameFn); else raf=null;
    };
    raf=requestAnimationFrame(frameFn);
  }
  assignTargets(modes[0]); place(1); draw();

  return {
    start(){ auto=true; if (!raf) loop(); },
    stop(){ auto=false; },
    setMode, next(){ setMode(idx+1); },
    get mode(){ return idx; },
    setTheme(name){ theme=THEMES[name]||theme; draw(); },
    destroy(){ auto=false; if (raf) cancelAnimationFrame(raf); raf=null; },
  };
}
