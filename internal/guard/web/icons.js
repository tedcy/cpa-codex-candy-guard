(()=>{var i=(a,r,o=[])=>{let f=document.createElementNS("http://www.w3.org/2000/svg",a);return Object.keys(r).forEach(t=>{f.setAttribute(t,String(r[t]))}),o.length&&o.forEach(t=>{let n=i(...t);f.appendChild(n)}),f},l=([a,r,o])=>i(a,r,o);var e={xmlns:"http://www.w3.org/2000/svg",width:24,height:24,viewBox:"0 0 24 24",fill:"none",stroke:"currentColor","stroke-width":2,"stroke-linecap":"round","stroke-linejoin":"round"};var s=["svg",e,[["path",{d:"M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"}],["polyline",{points:"10 17 15 12 10 7"}],["line",{x1:"15",x2:"3",y1:"12",y2:"12"}]]];var u=["svg",e,[["polygon",{points:"6 3 20 12 6 21 6 3"}]]];var d=["svg",e,[["path",{d:"M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"}],["path",{d:"M21 3v5h-5"}],["path",{d:"M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"}],["path",{d:"M8 16H3v5"}]]];var p=["svg",e,[["path",{d:"M15.2 3a2 2 0 0 1 1.4.6l3.8 3.8a2 2 0 0 1 .6 1.4V19a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z"}],["path",{d:"M17 21v-7a1 1 0 0 0-1-1H8a1 1 0 0 0-1 1v7"}],["path",{d:"M7 3v4a1 1 0 0 0 1 1h7"}]]];var m=["svg",e,[["path",{d:"M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"}],["path",{d:"m9 12 2 2 4-4"}]]];var x=["svg",e,[["rect",{width:"18",height:"18",x:"3",y:"3",rx:"2"}]]];var c={"shield-check":m,play:u,square:x,save:p,refresh:d,login:s};window.guardIcons=()=>document.querySelectorAll("[data-icon]").forEach(a=>{let r=c[a.dataset.icon];r&&!a.querySelector("svg")&&a.replaceChildren(l(r,{width:17,height:17,"aria-hidden":"true"}))});})();
/*! Bundled license information:

lucide/dist/esm/createElement.js:
lucide/dist/esm/defaultAttributes.js:
lucide/dist/esm/icons/log-in.js:
lucide/dist/esm/icons/play.js:
lucide/dist/esm/icons/refresh-cw.js:
lucide/dist/esm/icons/save.js:
lucide/dist/esm/icons/shield-check.js:
lucide/dist/esm/icons/square.js:
lucide/dist/esm/lucide.js:
  (**
   * @license lucide v0.468.0 - ISC
   *
   * This source code is licensed under the ISC license.
   * See the LICENSE file in the root directory of this source tree.
   *)
*/
