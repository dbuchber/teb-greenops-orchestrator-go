// Steuert das Challenge-Portal mit einem eigenen, dauerhaften Browserprofil.
//
//	node portal.mjs login                     # sichtbares Fenster: einmal selbst einloggen, dann Fenster schließen
//	node portal.mjs new-run [name] [tempo]    # legt einen Lauf an (Standard 720×) und lädt seine .env nach ~/Downloads
//	node portal.mjs export [ordner]           # alle Läufe als JSON und CSV (Standard: runs/ im Repo)
//	node portal.mjs explore [pfad] [knopf …]  # klickt die Knöpfe und listet Bedienelemente (nur Beschriftungen)
//
// Passwort und Sitzung bleiben im Profil (~/.greenops-browser); das Skript liest oder druckt sie nie.
// Beschriftungen, die wie Schlüssel aussehen, werden geschwärzt.
import { chromium } from 'playwright';
import os from 'node:os';
import path from 'node:path';
import { mkdir, writeFile } from 'node:fs/promises';

const PORTAL = process.env.GREENOPS_PORTAL ?? 'https://greenops.containerstack.net';
const PROFILE = path.join(os.homedir(), '.greenops-browser');
const DOWNLOADS = path.join(os.homedir(), 'Downloads');
const [mode = 'explore', target = '/', ...clicks] = process.argv.slice(2);

// csv flacht eine Liste von Objekten ab; verschachtelte Werte landen als JSON in der Zelle.
const csv = rows => {
	const cols = [...new Set(rows.flatMap(r => Object.keys(r)))];
	const cell = v => {
		const s = v == null ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v);
		return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
	};
	return [cols.join(','), ...rows.map(r => cols.map(c => cell(r[c])).join(','))].join('\n') + '\n';
};

const redact = s => s.replace(/\b[A-Za-z0-9_\-+/=]{24,}\b/g, '‹geschwärzt›').replace(/\s+/g, ' ').trim().slice(0, 120);

const ctx = await chromium.launchPersistentContext(PROFILE, {
	channel: 'chrome',
	headless: mode !== 'login',
	acceptDownloads: true,
	downloadsPath: DOWNLOADS,
});
const page = ctx.pages()[0] ?? (await ctx.newPage());

if (mode === 'login') {
	await page.goto(PORTAL);
	console.log('Bitte im Fenster einloggen und es danach schließen.');
	await new Promise(r => { ctx.on('close', r); page.on('close', r); }); // macOS: Fenster zu, Chrome läuft weiter
	await ctx.close();
	process.exit(0);
}

// Abgelaufene Portal-Sitzung: über die noch gültige GitHub-Sitzung ohne Passwort neu anmelden.
async function ensureLogin() {
	await page.goto(`${PORTAL}/app`, { waitUntil: 'networkidle' });
	if (!new URL(page.url()).pathname.startsWith('/app')) {
		await page.goto(`${PORTAL}/auth/github`, { waitUntil: 'networkidle' });
		if (!new URL(page.url()).pathname.startsWith('/app')) {
			throw new Error('nicht eingeloggt: einmal "node portal.mjs login" ausführen');
		}
	}
}

if (mode === 'new-run') {
	const name = target !== '/' ? target : `auto ${new Date().toTimeString().slice(0, 5)}`;
	const speed = clicks[0] ?? '720';
	await ensureLogin();
	await page.getByRole('button', { name: 'New simulation run' }).first().click();
	await page.getByPlaceholder('e.g. First green deployment').fill(name);
	const select = page.locator(`select:has(option:text-matches("^${speed}×"))`).first();
	const label = await select.locator('option', { hasText: new RegExp(`^${speed}×`) }).first().textContent();
	await select.selectOption({ label: label.trim() });
	await page.getByRole('button', { name: 'Create simulation run' }).click();
	await page.waitForURL(/\/app\/runs\/[0-9a-f]{12,}/, { timeout: 30_000 });
	const run = page.url().match(/\/app\/runs\/([0-9a-f]+)/)[1];
	console.log(`Lauf angelegt: ${run.slice(0, 10)} (${name}, ${speed}×)`);

	await page.goto(`${PORTAL}/app/access?run=${run}`, { waitUntil: 'networkidle' });
	await page.locator('summary', { hasText: 'Other credentials' }).click();
	const [dl] = await Promise.all([
		page.waitForEvent('download'),
		page.getByRole('button', { name: 'Download environment file' }).click(),
	]);
	const file = path.join(DOWNLOADS, dl.suggestedFilename());
	await dl.saveAs(file);
	console.log('Datei gespeichert:', file);
	await ctx.close();
	process.exit(0);
}

if (mode === 'export') {
	await ensureLogin();
	const api = async p => {
		const r = await ctx.request.get(PORTAL + p);
		if (!r.ok()) throw new Error(`${p}: ${r.status()}`);
		return r.json();
	};
	const out = path.resolve(target !== '/' ? target : '../../runs');
	const runs = await api('/api/runs');
	const summary = [];
	for (const { info, run } of runs) {
		const dir = path.join(out, info.id.slice(0, 10));
		await mkdir(dir, { recursive: true });
		const [full, history, alloc] = await Promise.all([
			api(`/api/runs/${info.id}`), api(`/api/runs/${info.id}/history`), api(`/api/runs/${info.id}/allocations`),
		]);
		const sim = full.run.simulation ?? {};
		const score = full.run.score ?? {};
		await writeFile(path.join(dir, 'run.json'), JSON.stringify(full, null, 1));
		await writeFile(path.join(dir, 'history.json'), JSON.stringify(history, null, 1));
		await writeFile(path.join(dir, 'allocations.json'), JSON.stringify(alloc, null, 1));
		await writeFile(path.join(dir, 'history.csv'), csv(history));
		await writeFile(path.join(dir, 'allocations.csv'), csv(alloc.intervals ?? []));
		await writeFile(path.join(dir, 'contributions.csv'), csv(score.contributions ?? []));
		await writeFile(path.join(dir, 'workload_impacts.csv'), csv(sim.workload_impacts ?? []));
		await writeFile(path.join(dir, 'hourly_impact.csv'), csv(sim.hourly_impact ?? []));
		summary.push({
			id: info.id.slice(0, 10), name: info.name, created_at: info.created_at, state: run.state,
			speed: run.speed_multiplier, final: score.final ?? '', total: score.total ?? '', base: score.base ?? '',
			bonus: score.bonus ?? '', usage_kg: score.usage_gco2e ? Number(score.usage_gco2e) / 1000 : '',
			total_kwh: sim.total_kwh ?? '', warnings: (full.run.warnings ?? []).length, violations: (full.run.violations ?? []).length,
		});
		console.log(`${info.id.slice(0, 10)}  ${run.state.padEnd(10)} ${String(score.total ?? '–').slice(0, 9).padStart(9)}  ${info.name}`);
	}
	await writeFile(path.join(out, 'runs.csv'), csv(summary));
	const lb = await api('/api/leaderboard').catch(e => ({ error: e.message }));
	await writeFile(path.join(out, 'leaderboard.json'), JSON.stringify(lb, null, 1));
	console.log('gespeichert in', out);
	await ctx.close();
	process.exit(0);
}

await page.goto(new URL(target, PORTAL).href, { waitUntil: 'networkidle' });
for (const c of clicks) {
	await page.getByRole('button', { name: c }).first().click();
	await page.waitForTimeout(1000);
}
console.log('URL:', page.url().replace(/([?&](token|code|state)=)[^&]+/g, '$1‹geschwärzt›'));
console.log('Titel:', await page.title());
const items = await page.$$eval('a, button, select, input, label, [role=button], [role=tab], [role=radio], [role=option], h1, h2, h3', els =>
	els.map(e => ({
		tag: e.tagName.toLowerCase(),
		type: e.getAttribute('type') ?? '',
		text: (e.innerText || e.getAttribute('aria-label') || e.getAttribute('name') || e.getAttribute('placeholder') || '').trim(),
		href: e.getAttribute('href') ?? '',
		options: e.tagName === 'SELECT' ? [...e.options].map(o => o.text) : [],
	})),
);
for (const i of items) {
	if (i.tag === 'input' && ['password', 'hidden'].includes(i.type)) continue;
	console.log(`${i.tag}${i.type ? `[${i.type}]` : ''}: ${redact(i.text)}${i.href ? ` → ${redact(i.href)}` : ''}${i.options.length ? ` {${i.options.join(' | ')}}` : ''}`);
}
await ctx.close();
