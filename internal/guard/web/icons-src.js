import { createElement, ShieldCheck, Play, Square, Save, RefreshCw, LogIn } from 'lucide';
const icons = { 'shield-check': ShieldCheck, play: Play, square: Square, save: Save, refresh: RefreshCw, login: LogIn };
window.guardIcons = () => document.querySelectorAll('[data-icon]').forEach(el => {
  const icon = icons[el.dataset.icon];
  if (icon && !el.querySelector('svg')) el.replaceChildren(createElement(icon, { width: 17, height: 17, 'aria-hidden': 'true' }));
});
