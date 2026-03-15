"use strict";
// Copy buttons
document.querySelectorAll('.btn-copy').forEach((btn) => {
    btn.addEventListener('click', () => {
        const targetId = btn.dataset.copy;
        if (!targetId)
            return;
        const el = document.getElementById(targetId);
        if (!el)
            return;
        const text = el.textContent || '';
        navigator.clipboard.writeText(text).then(() => {
            const original = btn.textContent;
            btn.textContent = '\u2713';
            setTimeout(() => { btn.textContent = original; }, 1200);
        });
    });
});
// Mobile nav toggle
const navToggle = document.querySelector('.nav-toggle');
const navLinks = document.getElementById('nav-links');
if (navToggle && navLinks) {
    navToggle.addEventListener('click', () => {
        const open = navLinks.classList.toggle('open');
        navToggle.setAttribute('aria-expanded', String(open));
    });
    // Close on nav link click
    navLinks.querySelectorAll('.nav-link').forEach((link) => {
        link.addEventListener('click', () => {
            navLinks.classList.remove('open');
            navToggle.setAttribute('aria-expanded', 'false');
        });
    });
}
// Theme toggle
const themeToggle = document.querySelector('.theme-toggle');
if (themeToggle) {
    themeToggle.addEventListener('click', () => {
        const current = document.documentElement.getAttribute('data-theme');
        // If no explicit theme, detect current effective theme
        const isDark = current === 'dark' || (!current && !window.matchMedia('(prefers-color-scheme: light)').matches);
        const next = isDark ? 'light' : 'dark';
        document.documentElement.setAttribute('data-theme', next);
        localStorage.setItem('theme', next);
    });
}
function refreshServerStatus() {
    fetch('/api/servers')
        .then((r) => r.json())
        .then((servers) => {
        const byLabel = new Map(servers.map((s) => [s.label, s]));
        document.querySelectorAll('.server-row[data-server]').forEach((row) => {
            const label = row.dataset.server;
            if (!label)
                return;
            const info = byLabel.get(label);
            if (!info)
                return;
            const dot = row.querySelector('.server-status-dot');
            const detail = row.querySelector('.server-detail');
            if (!dot || !detail)
                return;
            dot.classList.remove('online', 'offline');
            dot.classList.add(info.online ? 'online' : 'offline');
            if (info.online) {
                const mapHtml = info.type !== 'ts3' && info.map
                    ? `<span class="server-map">${escapeHtml(info.map)}</span>`
                    : '';
                detail.innerHTML = mapHtml +
                    `<span class="server-players">${info.players}/${info.maxPlayers}</span>`;
            }
            else {
                detail.innerHTML = '<span class="server-offline-text">Offline</span>';
            }
        });
    })
        .catch(() => { });
}
function escapeHtml(s) {
    const el = document.createElement('span');
    el.textContent = s;
    return el.innerHTML;
}
// Poll every 60s if the servers card is present.
if (document.getElementById('servers-card')) {
    setInterval(refreshServerStatus, 60000);
}
// YouTube facade — click to load iframe
document.querySelectorAll('.yt-facade').forEach((el) => {
    const activate = () => {
        const id = el.dataset.videoId;
        if (!id)
            return;
        const iframe = document.createElement('iframe');
        iframe.src = `https://www.youtube-nocookie.com/embed/${id}?autoplay=1`;
        iframe.setAttribute('frameborder', '0');
        iframe.setAttribute('allowfullscreen', '');
        iframe.allow = 'accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture';
        el.textContent = '';
        el.classList.remove('yt-facade');
        el.appendChild(iframe);
    };
    el.addEventListener('click', activate);
    el.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            activate();
        }
    });
});
