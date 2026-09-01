import { AlertCenter } from './js/alert-center.js';
import { DashboardPanel } from './js/dashboard.js';
import { HistoryDialog } from './js/history-dialog.js';
import { UsagePanel } from './js/usage.js';
import { $, activateToggle } from './js/shared.js';

document.documentElement.classList.toggle('iframe-embedded', window.self !== window.top);

let dashboard;
const history = new HistoryDialog();
dashboard = new DashboardPanel((target, name) => history.open(target, name));
const usage = new UsagePanel();
const alerts = new AlertCenter();
let activePanel = 'dashboard';
const tabButtons = [...document.querySelectorAll('[data-monitor-tab]')];
const panels = {
  dashboard: $('#dashboardPanel'),
  usage: $('#usageSection'),
};

tabButtons.forEach((button, index) => {
  button.addEventListener('click', () => selectPanel(button.dataset.monitorTab));
  button.addEventListener('keydown', (event) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const nextIndex = event.key === 'Home' ? 0
      : event.key === 'End' ? tabButtons.length - 1
        : (index + (event.key === 'ArrowRight' ? 1 : -1) + tabButtons.length) % tabButtons.length;
    tabButtons[nextIndex].focus();
    selectPanel(tabButtons[nextIndex].dataset.monitorTab);
  });
});

function selectPanel(panelName) {
  if (panelName === activePanel) return;
  activePanel = panelName;
  tabButtons.forEach((button) => {
    const selected = button.dataset.monitorTab === panelName;
    button.setAttribute('aria-selected', String(selected));
    button.tabIndex = selected ? 0 : -1;
  });
  Object.entries(panels).forEach(([name, panel]) => {
    panel.hidden = name !== panelName;
  });
  window.scrollTo(0, 0);
  refreshActivePanel();
}

function loadActivePanel(options) {
  return activePanel === 'dashboard' ? dashboard.load(options) : usage.load(options);
}

function refreshActivePanel() {
  void loadActivePanel();
  if (activePanel === 'dashboard') void dashboard.loadActivity();
}

document.querySelectorAll('[data-filter]').forEach((button) => {
  button.addEventListener('click', () => {
    activateToggle('[data-filter]', button);
    dashboard.setFilter(button.dataset.filter);
  });
});

$('#usagePeriodSelect').addEventListener('change', (event) => {
  usage.setPeriod(event.target.value);
  void usage.load();
});

$('#refreshButton').addEventListener('click', () => {
  refreshActivePanel();
});

refreshActivePanel();
void alerts.load();

setInterval(() => void loadActivePanel({ silent: true }), 30000);
setInterval(() => {
  if (activePanel === 'dashboard') void dashboard.loadActivity({ silent: true });
}, 10000);
setInterval(() => void alerts.load(), 30000);
