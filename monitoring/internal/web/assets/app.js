import { AlertCenter } from './js/alert-center.js';
import { DashboardPanel } from './js/dashboard.js';
import { HistoryDialog } from './js/history-dialog.js';
import { UsagePanel } from './js/usage.js';
import { $, activateToggle } from './js/shared.js';

let dashboard;
const history = new HistoryDialog();
dashboard = new DashboardPanel((target, name) => history.open(target, name));
const usage = new UsagePanel();
const alerts = new AlertCenter();

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
  void dashboard.load();
  void dashboard.loadActivity();
  void usage.load();
});

void dashboard.load();
void dashboard.loadActivity();
void usage.load();
void alerts.load();

setInterval(() => {
  void dashboard.load();
  void usage.load({ silent: true });
}, 30000);
setInterval(() => void dashboard.loadActivity({ silent: true }), 10000);
setInterval(() => void alerts.load(), 30000);
