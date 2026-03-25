// ── read state (localStorage) ────────────────────────────────────────────────
const READ_KEY = 'kijiyomu_read';
const READ_TTL = 7 * 24 * 60 * 60 * 1000; // 7日

function getReadMap() {
  try { return JSON.parse(localStorage.getItem(READ_KEY) || '{}'); }
  catch { return {}; }
}
function getReadSet() {
  const now = Date.now(), map = getReadMap();
  return new Set(Object.entries(map).filter(([, ts]) => now - ts < READ_TTL).map(([u]) => u));
}
function saveRead(url) {
  const now = Date.now(), map = getReadMap();
  map[url] = now;
  // 期限切れを削除
  for (const [u, ts] of Object.entries(map)) {
    if (now - ts >= READ_TTL) delete map[u];
  }
  localStorage.setItem(READ_KEY, JSON.stringify(map));
}

// ページ読み込み時: 既読をグレーにして末尾へ移動、デフォルトは非表示
let showRead = false;

(function initRead() {
  const read = getReadSet();
  const grid = document.getElementById('articles');
  const cards = Array.from(grid.querySelectorAll('.card'));
  const readCards = [];
  cards.forEach(card => {
    if (read.has(card.dataset.url)) {
      card.classList.add('read', 'read-hidden');
      readCards.push(card);
    }
  });
  readCards.forEach(card => grid.appendChild(card));
  updateReadBtn();
  updateMarkAllBar();
})();

function updateReadBtn() {
  const read = getReadSet();
  const btn = document.getElementById('read-toggle');
  if (!btn) return;
  const count = document.querySelectorAll('.card.read').length;
  btn.textContent = showRead ? '既読を隠す (' + count + ')' : '既読も表示 (' + count + ')';
  btn.classList.toggle('active', showRead);
}

function toggleShowRead() {
  showRead = !showRead;
  document.querySelectorAll('.card.read').forEach(c => {
    c.classList.toggle('read-hidden', !showRead);
  });
  updateReadBtn();
  applyThreshold(); // 表示件数を再計算
}

function updateMarkAllBar() {
  const bar = document.getElementById('mark-all-bar');
  if (!bar) return;
  const hasUnread = document.querySelector('.card:not(.read)') !== null;
  bar.classList.toggle('hidden', !hasUnread);
}

function markAllAsRead() {
  document.querySelectorAll('.card:not(.read)').forEach(card => {
    card.classList.add('read');
    if (!showRead) card.classList.add('read-hidden');
    saveRead(card.dataset.url);
  });
  updateMarkAllBar();
  updateReadBtn();
  applyThreshold();
}

// 一度ビューポートに入ったカードが画面外に出たら既読に
const seenCards = new Set();
const readObserver = new IntersectionObserver(entries => {
  entries.forEach(entry => {
    const card = entry.target;
    if (card.classList.contains('read')) return;
    if (entry.isIntersecting) {
      seenCards.add(card);
    } else if (seenCards.has(card)) {
      seenCards.delete(card);
      card.classList.add('read'); // グレーにするだけ。今セッションは消さない
      saveRead(card.dataset.url);
      updateReadBtn();
      updateMarkAllBar();
    }
  });
}, { threshold: 0.1 });

document.querySelectorAll('.card[data-url]').forEach(c => readObserver.observe(c));

// ── keyboard navigation ──────────────────────────────────────────────────────
let selectedIdx = -1;

function visibleCards() {
  return Array.from(document.querySelectorAll('.card:not(.hidden)'));
}

function colCount() {
  const cards = visibleCards();
  if (cards.length < 2) return 1;
  const top0 = cards[0].getBoundingClientRect().top;
  let n = 1;
  for (let i = 1; i < cards.length; i++) {
    if (Math.abs(cards[i].getBoundingClientRect().top - top0) < 4) n++;
    else break;
  }
  return n;
}

function selectCard(idx) {
  const cards = visibleCards();
  if (!cards.length) return;
  // clamp
  idx = Math.max(0, Math.min(idx, cards.length - 1));
  if (selectedIdx >= 0 && selectedIdx < cards.length)
    cards[selectedIdx].classList.remove('selected');
  selectedIdx = idx;
  const card = cards[selectedIdx];
  card.classList.add('selected');
  card.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

document.addEventListener('keydown', e => {
  if (e.target.tagName === 'INPUT' || e.metaKey || e.ctrlKey) return;
  const cards = visibleCards();
  if (!cards.length) return;
  const cols = colCount();
  switch (e.key) {
    case 'j': case 'J':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx + cols); break;
    case 'k': case 'K':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx - cols); break;
    case 'h': case 'H':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx - 1); break;
    case 'l': case 'L':
      e.preventDefault();
      selectCard(selectedIdx < 0 ? 0 : selectedIdx + 1); break;
    case 'Enter':
      if (selectedIdx >= 0 && selectedIdx < cards.length) {
        const a = cards[selectedIdx].querySelector('.card-title a');
        if (a) window.open(a.href, '_blank', 'noopener');
      }
      break;
  }
});

// フィルター変更時に選択リセット
function resetSelection() { selectedIdx = -1; }

// ── source filter / threshold ────────────────────────────────────────────────
let currentSource = 'all';
function filterSource(src) {
  currentSource = src;
  resetSelection();
  document.querySelectorAll('.filter-btn').forEach(b => {
    b.classList.toggle('active', b.textContent.trim() === (src === 'all' ? 'All' : src));
  });
  applyThreshold();
}
function applyThreshold() {
  resetSelection();
  const th = parseInt(document.getElementById('threshold').value) || 0;
  let count = 0;
  document.querySelectorAll('.card').forEach(card => {
    const srcOk = currentSource === 'all' || card.dataset.source === currentSource;
    const scoreOk = (parseInt(card.dataset.score) || 0) >= th;
    const readOk = showRead || !card.classList.contains('read-hidden');
    const show = srcOk && scoreOk && readOk;
    card.classList.toggle('hidden', !show);
    if (show) count++;
  });
  document.getElementById('visible-count').textContent = count;
}
