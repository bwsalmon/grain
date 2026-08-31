/**
 * grain-mark.js — Grain brand mark renderer (final, pack build 35)
 *
 * Vendored from the grain-mark design pack. Kept as close to the pack's
 * own copy as possible so a later revision of the mark drops in as a
 * diff rather than a port; the deviations from it are:
 *
 *   - `sizePx` (below) picks the tier. The pack keys its tiers, its
 *     per-figure zooms and its tiny-glyph chain filters off `canvas.width`,
 *     which on a 2x display is twice the size the mark is *seen* at -- a
 *     24px mark would back onto a 48px canvas and render as the full tier,
 *     which is the wrong figure, not merely the wrong density. Passing the
 *     CSS size as `sizePx` decides the tier by what the reader sees and
 *     leaves the backing store free to be as dense as the display allows.
 *     Left unset it is `canvas.width`, exactly the pack's own behaviour.
 *   - `frameWidth` is an option instead of a per-style constant, so the
 *     animated mark and the static glyph can be told to draw the same
 *     ring. Left unset, both draw the widths the pack hard-codes (0.05 of
 *     the width for the glyph export, 0.012 for the animation), which at
 *     grain's icon sizes differ enough that switching between them reads
 *     as the ring changing rather than the figure animating.
 *   - `slot` is the cycle slot the animation opens on, so it can be
 *     started on the figure the still was already showing. The pack always
 *     opens on slot 0.
 *
 * See docs/brand.md for what the mark means and where grain uses it.
 *
 * Chladni-plate resonance figures in a circular frame. Grains sit evenly along
 * the nodal lines of a square-plate eigenmode and fly between figures on mode
 * changes. Two tiers:
 *   - full tier (>=40px): the main cycle, identical figure at every size
 *   - tiny tier  (<40px): a companion cycle of four simplified glyphs,
 *     changing in lockstep with the main cycle
 *
 * ES module, no dependencies.
 *
 *   import { createGrainMark, renderStatic, MODES, TINY_MODES } from './grain-mark.js';
 *   const mark = createGrainMark(canvas, { theme:'light' });
 *   mark.start();          // auto-cycle
 *   mark.setMode(2);       // snap to cycle slot 2 (tiny canvases show their companion glyph)
 *   renderStatic(iconCanvas, { slot: 3, theme: 'dark' });
 */

// ---------- tuned constants (design session, build 35) ----------
export const DEFAULTS = {
  snapSeconds:  1.3,     // grain flight time between figures
  dwellSeconds: 0.33,    // hold on each figure
  scatter:      0.07,    // fraction of frame radius grains stray mid-flight
  zoom:         1.0,
  shape:        'circle',
  theme:        'dark',
  sizePx:       null,    // CSS size the mark is seen at; null = canvas.width
  frameWidth:   null,    // ring width as a fraction of canvas width; null = per-style default
  slot:         0,       // cycle slot the animation opens on
};

// Main cycle [n, m, sign]; sign -1 antisymmetric (has diagonals), +1 symmetric
export const MODES      = [[1,4,-1],[2,3,1],[2,3,-1],[1,4,1]];
// Tiny-tier companion glyphs, one per slot: X-and-arcs, diamond, rosette, plus
export const TINY_MODES = [[1,2,-1],[1,2,1],[2,3,-1],[2,3,1]];

export const THEMES = {
  dark:  { grain:'#D9A441', grainRGB:[0xD9,0xA4,0x41], border:'rgba(217,164,65,0.9)' },
  light: { grain:'#8A6A2E', grainRGB:[0x8A,0x6A,0x2E], border:'rgba(138,106,46,0.95)' },
};

// Rendering tiers by canvas pixel size
export function tierFor(W){
  if (W >= 300) return { style:'grains', count: 2000, radius: 0.0065, jitter: 0.9, tiny:false };
  if (W >= 80)  return { style:'grains', count: 520,  radius: 0.013,  jitter: 0,   tiny:false };
  if (W >= 40)  return { style:'grains', count: 520,  radius: 0.017,  jitter: 0,   tiny:false };
  return { style:'grains', count: Math.round(W*3.75), radius: 1.05/W, jitter: 0, tiny:true };
}

// Per-figure zoom at tiny sizes (>1 crops in, <1 pulls out)
export function modeZoom(n, m, sg, W){
  if (W < 40 && n===1 && m===3 && sg<0) return 1.5;   // square-and-cross: corners off-frame (kept for reference)
  if (W < 40 && n===2 && m===3 && sg<0) return 0.78;  // rosette: zoomed out, X removed by chain filter
  if (W < 40 && n===2 && m===3 && sg>0) return 1.5;   // plus fills the circle
  return 1;
}

// Per-figure chain filter at tiny sizes, by radial extent relative to frame R.
// `tiny` is the pack's `frameR < 18` test, hoisted to the caller so it can be
// answered in CSS pixels; minR/maxR stay in real pixels and compare against the
// real frameR, so the ratios are unaffected by the backing store's scale.
function tinyKeepChain(n, m, sg, tiny, frameR, minR, maxR){
  if (tiny && n===2 && m===3 && sg>0) return maxR < frameR*0.97;  // plus + center ring; square off-frame
  if (tiny && n===2 && m===3 && sg<0) return minR > frameR*0.15;  // lobes only; the X is the sole chain through center
  return true;
}

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

export function makeFrame(cx, cy, R, shape){
  const verts=[];
  for (let i=0;i<6;i++){
    const a=-Math.PI/2+i*Math.PI/3;
    verts.push({x:cx+Math.cos(a)*R, y:cy+Math.sin(a)*R});
  }
  return {
    cx, cy, R, shape, verts,
    path(ctx){
      ctx.beginPath();
      if (shape==='circle') ctx.arc(cx,cy,R,0,Math.PI*2);
      else verts.forEach((p,i)=> i===0?ctx.moveTo(p.x,p.y):ctx.lineTo(p.x,p.y));
      ctx.closePath();
    },
    contains(x,y){
      if (shape==='circle'){ const dx=x-cx,dy=y-cy; return dx*dx+dy*dy<=R*R; }
      let s0=null;
      for (let i=0;i<6;i++){
        const a=verts[i], b=verts[(i+1)%6];
        const c=(b.x-a.x)*(y-a.y)-(b.y-a.y)*(x-a.x);
        const s=c>0;
        if (s0===null) s0=s; else if (s!==s0) return false;
      }
      return true;
    },
  };
}

/** Marching squares over the field, clipped to the frame. */
function rawSegments(frame, Rfield, mode, res){
  const [n,m,sign]=mode;
  const span=frame.R*1.05, step=span*2/res;
  const sx=frame.cx-span, sy=frame.cy-span;
  const vals=[];
  for (let j=0;j<=res;j++){
    const row=[];
    for (let i=0;i<=res;i++){
      const x=sx+i*step, y=sy+j*step;
      row.push(fieldVal((x-frame.cx)/Rfield,(y-frame.cy)/Rfield,n,m,sign));
    }
    vals.push(row);
  }
  const segs=[];
  for (let j=0;j<res;j++){
    for (let i=0;i<res;i++){
      const x0=sx+i*step, y0=sy+j*step;
      const vTL=vals[j][i], vTR=vals[j][i+1], vBR=vals[j+1][i+1], vBL=vals[j+1][i];
      let c=0;
      if (vTL>=0) c|=1; if (vTR>=0) c|=2; if (vBR>=0) c|=4; if (vBL>=0) c|=8;
      for (const [a,b] of SEG[c]){
        const ep=(idx)=>{
          if (idx===0){ const t=vTL/(vTL-vTR); return {x:x0+t*step,y:y0}; }
          if (idx===1){ const t=vTR/(vTR-vBR); return {x:x0+step,y:y0+t*step}; }
          if (idx===2){ const t=vBR/(vBR-vBL); return {x:x0+step-t*step,y:y0+step}; }
          const t=vBL/(vBL-vTL); return {x:x0,y:y0+step-t*step};
        };
        const p1=ep(a), p2=ep(b);
        if (!frame.contains((p1.x+p2.x)/2,(p1.y+p2.y)/2)) continue;
        const len=Math.hypot(p2.x-p1.x,p2.y-p1.y);
        if (len>1e-6) segs.push({p1,p2,len});
      }
    }
  }
  return { segs, step };
}

/**
 * Full figure pipeline: trace -> prune edge bumps -> trail-decompose into
 * chains (covering every segment) -> per-figure tiny filters -> N points at
 * even arc length per chain, allocated by length.
 * Returns points; .chains holds the polylines for stroke rendering.
 *
 * `designR` is the frame radius at the size the mark is *seen* at, and only
 * ever answers the two "is this the tiny tier" questions below; every
 * measurement stays in real pixels. It defaults to the real radius, which is
 * the pack's own behaviour.
 */
export function figurePoints(frame, Rfield, mode, N, res, jitter, designR){
  const [n,m,sign]=mode;
  const R = designR ?? frame.R;
  const tinyGlyph = R < 18;
  let { segs, step } = rawSegments(frame, Rfield, mode, res);
  const q=step/8;                              // endpoint quantum scales with the cell (fixes sub-pixel crumb chains)
  const key=(p)=>Math.round(p.x/q)+','+Math.round(p.y/q);

  // prune chains hugging the frame edge
  {
    const adj=new Map();
    segs.forEach((sg,i)=>{ for (const k of [key(sg.p1),key(sg.p2)]){ if(!adj.has(k)) adj.set(k,[]); adj.get(k).push(i);} });
    const seen=new Array(segs.length).fill(false);
    const keep=[];
    const tiny = R < 40;
    for (let i=0;i<segs.length;i++){
      if (seen[i]) continue;
      const comp=[]; const st=[i]; seen[i]=true;
      while (st.length){
        const c=st.pop(); comp.push(c);
        for (const k of [key(segs[c].p1),key(segs[c].p2)]) for (const j of adj.get(k)) if(!seen[j]){seen[j]=true;st.push(j);}
      }
      let len=0, maxInward=0;
      for (const c of comp){
        len+=segs[c].len;
        for (const p of [segs[c].p1,segs[c].p2]){
          const d=frame.R-Math.hypot(p.x-frame.cx,p.y-frame.cy);
          if (d>maxInward) maxInward=d;
        }
      }
      const isBump = tiny ? (maxInward < frame.R*0.08)
                          : (maxInward < frame.R*0.16 && len < frame.R*1.2);
      if (!isBump) comp.forEach(c=>keep.push(segs[c]));
    }
    segs=keep;
  }

  // trail decomposition covering every segment; crossings spawn further trails
  const adj=new Map();
  segs.forEach((sg,i)=>{ for (const k of [key(sg.p1),key(sg.p2)]){ if(!adj.has(k)) adj.set(k,[]); adj.get(k).push(i);} });
  const visited=new Array(segs.length).fill(false);
  const remDeg=(k)=>adj.get(k).reduce((a,j)=>a+(visited[j]?0:1),0);
  const chains=[];
  let remaining=segs.length;
  while (remaining>0){
    let startKey=null;
    for (const [k] of adj){ if (remDeg(k)===1){ startKey=k; break; } }
    if (startKey===null){ const i=visited.findIndex(v=>!v); startKey=key(segs[i].p1); }
    const firstSeg=adj.get(startKey).find(j=>!visited[j]);
    const fs=segs[firstSeg];
    let at = key(fs.p1)===startKey ? fs.p1 : fs.p2;
    const chain=[at];
    while (true){
      const nextIdx=adj.get(key(at)).find(j=>!visited[j]);
      if (nextIdx===undefined) break;
      visited[nextIdx]=true; remaining--;
      const sg=segs[nextIdx];
      at = key(sg.p1)===key(at) ? sg.p2 : sg.p1;
      chain.push(at);
    }
    let len=0, minR=Infinity, maxR=0;
    for (let i2=1;i2<chain.length;i2++) len+=Math.hypot(chain[i2].x-chain[i2-1].x, chain[i2].y-chain[i2-1].y);
    for (const pt of chain){
      const d=Math.hypot(pt.x-frame.cx,pt.y-frame.cy);
      if (d<minR) minR=d; if (d>maxR) maxR=d;
    }
    if (len>1e-6 && tinyKeepChain(n,m,sign,tinyGlyph,frame.R,minR,maxR)) chains.push({chain,len});
  }

  // drop artifact chains shorter than ~1.5 grain spacings, then allocate by length
  let totalLen=chains.reduce((a,b)=>a+b.len,0);
  const minChain=totalLen/N*1.5;
  const kept=chains.filter(c=>c.len>=minChain);
  const finalChains = kept.length?kept:chains;
  totalLen=finalChains.reduce((a,b)=>a+b.len,0);

  const pts=[];
  pts.chains=finalChains;
  if (!finalChains.length) return pts;
  let alloc=finalChains.map(c=>({c, n:Math.floor(N*c.len/totalLen), frac:(N*c.len/totalLen)%1}));
  let used=alloc.reduce((a,b)=>a+b.n,0);
  alloc.sort((a,b)=>b.frac-a.frac);
  for (let i=0; used<N && i<alloc.length; i++,used++) alloc[i].n++;
  for (const a of alloc){
    const {chain,len}=a.c;
    const nn=Math.max(2,a.n);
    for (let qi=0; qi<nn && pts.length<N; qi++){
      let d=(qi+0.5)*len/nn;
      let seg=1;
      while (seg<chain.length){
        const l=Math.hypot(chain[seg].x-chain[seg-1].x, chain[seg].y-chain[seg-1].y);
        if (d<=l || seg===chain.length-1){
          const f=l>1e-9?Math.min(1,d/l):0;
          const dx=chain[seg].x-chain[seg-1].x, dy=chain[seg].y-chain[seg-1].y;
          const nl=Math.hypot(dx,dy)||1;
          const jx=(-dy/nl)*(Math.random()*2-1)*(jitter||0);
          const jy=( dx/nl)*(Math.random()*2-1)*(jitter||0);
          pts.push({x:chain[seg-1].x+dx*f+jx, y:chain[seg-1].y+dy*f+jy});
          break;
        }
        d-=l; seg++;
      }
    }
  }
  return pts;
}

// ---------- rendering ----------
function drawFrameBorder(ctx, frame, W, theme, widthFrac){
  ctx.strokeStyle=theme.border;
  ctx.lineWidth=Math.max(1,W*widthFrac);
  frame.path(ctx); ctx.stroke();
}

function figureForSlot(slot, tiny){
  const list = tiny ? TINY_MODES : MODES;
  return list[((slot%list.length)+list.length)%list.length];
}

/** Static render of a cycle slot (or explicit mode). style: 'grains'|'strokes'|'filled'. */
export function renderStatic(canvas, opts={}){
  const o={...DEFAULTS,...opts};
  const ctx=canvas.getContext('2d');
  const W=canvas.width, H=canvas.height;
  const cx=W/2, cy=H/2, Rframe=Math.min(W,H)*0.44;
  const frame=makeFrame(cx,cy,Rframe,o.shape);
  const theme=THEMES[o.theme]||THEMES.dark;
  const S=o.sizePx ?? W;                 // the size the mark is seen at
  const designR=S*0.44;
  const tier=tierFor(S);
  const mode = o.mode || figureForSlot(o.slot??0, tier.tiny);
  const Rfield = Rframe*o.zoom*modeZoom(mode[0],mode[1],mode[2],S);
  const style=o.style||tier.style;
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
      d[i]=rgb[0]; d[i+1]=rgb[1]; d[i+2]=rgb[2]; d[i+3]=Math.round(cover/9*255);
    }
    ctx.putImageData(img,0,0);
    drawFrameBorder(ctx,frame,W,theme,o.frameWidth ?? 0.05);
    return;
  }

  const pts=figurePoints(frame,Rfield,mode, o.count??tier.count, 64, o.jitter??tier.jitter, designR);
  ctx.save(); frame.path(ctx); ctx.clip();
  if (style==='strokes'){
    ctx.strokeStyle=theme.grain; ctx.lineWidth=Math.max(0.75,W*0.028);
    ctx.lineCap='round'; ctx.lineJoin='round';
    for (const c of pts.chains){
      ctx.beginPath();
      c.chain.forEach((p,i)=> i===0?ctx.moveTo(p.x,p.y):ctx.lineTo(p.x,p.y));
      ctx.stroke();
    }
  } else {
    ctx.fillStyle=theme.grain;
    const r=Math.max(0.6,W*(o.radius??tier.radius));
    for (const p of pts){ ctx.beginPath(); ctx.arc(p.x,p.y,r,0,Math.PI*2); ctx.fill(); }
  }
  ctx.restore();
  drawFrameBorder(ctx,frame,W,theme,o.frameWidth ?? 0.012);
}

/** Animated mark. Tiny canvases (<40px) automatically run the companion glyph cycle. */
export function createGrainMark(canvas, opts={}){
  const o={...DEFAULTS,...opts};
  const ctx=canvas.getContext('2d');
  const W=canvas.width, H=canvas.height;
  const cx=W/2, cy=H/2, Rframe=Math.min(W,H)*0.44;
  const frame=makeFrame(cx,cy,Rframe,o.shape);
  const S=o.sizePx ?? W;                 // the size the mark is seen at
  const designR=S*0.44;
  const tier=tierFor(S);
  const count=o.count??tier.count, radius=o.radius??tier.radius, jitter=o.jitter??tier.jitter;
  let theme=THEMES[o.theme]||THEMES.dark;

  const grains=[];
  for (let i=0;i<count;i++) grains.push({x:cx,y:cy,sx:cx,sy:cy,tx:cx,ty:cy,kx:0,ky:0});

  function assignTargets(slot){
    const mode=figureForSlot(slot,tier.tiny);
    const Rfield=Rframe*o.zoom*modeZoom(mode[0],mode[1],mode[2],S);
    const targets=figurePoints(frame,Rfield,mode,count,64,jitter,designR);
    // greedy nearest matching with spatial buckets
    const cell=Math.max(4,Rframe*0.08), cols=Math.ceil(W/cell), rows=Math.ceil(H/cell);
    const buckets=Array.from({length:cols*rows},()=>[]);
    grains.forEach((g,i)=>{
      const bx=Math.min(cols-1,Math.max(0,Math.floor(g.x/cell)));
      const by=Math.min(rows-1,Math.max(0,Math.floor(g.y/cell)));
      buckets[by*cols+bx].push(i);
    });
    for (const t of targets.slice().sort(()=>Math.random()-0.5)){
      const bx=Math.min(cols-1,Math.max(0,Math.floor(t.x/cell)));
      const by=Math.min(rows-1,Math.max(0,Math.floor(t.y/cell)));
      let best=-1,bd=Infinity,bb=null,bp=-1;
      for (let ring=0; ring<Math.max(cols,rows); ring++){
        for (let yy=by-ring; yy<=by+ring; yy++){ if (yy<0||yy>=rows) continue;
          for (let xx=bx-ring; xx<=bx+ring; xx++){ if (xx<0||xx>=cols) continue;
            if (Math.abs(yy-by)!==ring && Math.abs(xx-bx)!==ring) continue;
            const b=buckets[yy*cols+xx];
            for (let qi=0;qi<b.length;qi++){
              const g=grains[b[qi]];
              const d=(g.x-t.x)**2+(g.y-t.y)**2;
              if (d<bd){bd=d;best=b[qi];bb=b;bp=qi;}
            }
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
    for (const g of grains){
      g.x=g.sx+(g.tx-g.sx)*k+g.kx*bulge;
      g.y=g.sy+(g.ty-g.sy)*k+g.ky*bulge;
    }
  }
  function draw(){
    ctx.clearRect(0,0,W,H);
    ctx.save(); frame.path(ctx); ctx.clip();
    ctx.fillStyle=theme.grain;
    const r=Math.max(0.6,W*radius);
    for (const g of grains){ ctx.beginPath(); ctx.arc(g.x,g.y,r,0,Math.PI*2); ctx.fill(); }
    ctx.restore();
    drawFrameBorder(ctx,frame,W,theme,o.frameWidth ?? 0.012);
  }

  let idx=((o.slot%MODES.length)+MODES.length)%MODES.length;
  let snapT=1, dwellT=0, auto=false, raf=null, last=0;
  function setMode(i){
    idx=((i%MODES.length)+MODES.length)%MODES.length;
    assignTargets(idx);
    snapT=0; dwellT=0;
    if (!raf) loop();
  }
  function loop(){
    last=performance.now();
    const frameFn=(now)=>{
      const dt=Math.min(0.05,(now-last)/1000); last=now;
      if (snapT<1){
        snapT=Math.min(1,snapT+dt/o.snapSeconds);
        const e=snapT<0.5 ? 4*snapT**3 : 1-Math.pow(-2*snapT+2,3)/2;
        place(e);
        draw();
      } else if (auto){
        dwellT+=dt;
        if (dwellT>=o.dwellSeconds) setMode(idx+1);
      }
      if (auto || snapT<1) raf=requestAnimationFrame(frameFn); else raf=null;
    };
    raf=requestAnimationFrame(frameFn);
  }
  assignTargets(idx); place(1); draw();

  return {
    start(){ auto=true; if (!raf) loop(); },
    stop(){ auto=false; },
    setMode, next(){ setMode(idx+1); },
    get mode(){ return idx; },
    setTheme(name){ theme=THEMES[name]||theme; draw(); },
    destroy(){ auto=false; if (raf) cancelAnimationFrame(raf); raf=null; },
  };
}
