import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { boundPattern, refineDashboard, signedNetworkTraffic, sizeBounds, sizeBucketTargets } from './refine-dashboards.mjs';

const files = [
  new URL('./grafana-overview.json', import.meta.url),
  new URL('./grafana-dashboard.json', import.meta.url),
];
const flatten = panels => panels.flatMap(panel => [panel, ...flatten(panel.panels || [])]);

test('published dashboards use portable data sources and neutral selections', () => {
  for (const file of files) {
    const dashboard = JSON.parse(readFileSync(file, 'utf8'));
    assert.equal(dashboard.id, null);
    for (const variable of dashboard.templating.list) {
      if (variable.current) assert.equal(variable.current.value, '$__all');
    }
    const inspect = value => {
      if (!value || typeof value !== 'object') return;
      if (value.type === 'prometheus' && Object.hasOwn(value, 'uid')) {
        assert.equal(value.uid, '${DS_PROMETHEUS}');
      }
      for (const child of Object.values(value)) inspect(child);
    };
    inspect(dashboard);
  }
});

test('network receive is negative, transmit positive, and the axis is unconstrained', () => {
  for (const file of [files[0]]) {
    const source = JSON.parse(readFileSync(file, 'utf8'));
    const before = flatten(source.panels).find(panel => panel.targets?.some(target => target.expr?.includes('node_network_receive')));
    const transmit = before.targets.find(target => target.expr.includes('node_network_transmit')).expr;
    const result = signedNetworkTraffic(structuredClone(source));
    const panel = flatten(result.panels).find(panel => panel.id === before.id);
    assert.match(panel.targets.find(target => target.expr.includes('node_network_receive')).expr, /^-vllm_dashboard_node_network_receive/);
    assert.equal(panel.targets.find(target => target.expr.includes('node_network_transmit')).expr, transmit);
    assert(!transmit.startsWith('-'));
    assert.equal(panel.fieldConfig.defaults.min, undefined);
    assert.equal(panel.fieldConfig.defaults.unit, 'Bps');
    assert.deepEqual(signedNetworkTraffic(structuredClone(result)), result);
  }
});

for (const file of files) {
  test(`refine ${file.pathname.split('/').slice(-3).join('/')}`, () => {
    const source = JSON.parse(readFileSync(file, 'utf8'));
    const before = structuredClone(source);
    const dashboard = refineDashboard(source);
    assert.deepEqual(source, before);
    assert.deepEqual(refineDashboard(dashboard), dashboard);
    const panels = flatten(dashboard.panels);
    assert.equal(new Set(panels.map(panel => panel.id)).size, panels.length);
    const charts = panels.filter(panel => panel.type !== 'row');
    for (const [index, panel] of charts.entries()) {
      for (const target of panel.targets || []) {
        if (target.expr) assert(target.legendFormat?.trim(), `${panel.title} has an unnamed query`);
      }
      const rectangle = panel.gridPos;
      assert(rectangle.x >= 0 && rectangle.x + rectangle.w <= 24);
      for (const other of charts.slice(index + 1)) {
        const box = other.gridPos;
        assert(!(rectangle.x < box.x + box.w && rectangle.x + rectangle.w > box.x && rectangle.y < box.y + box.h && rectangle.y + rectangle.h > box.y), `${panel.title} overlaps ${other.title}`);
      }
      if (panel.type === 'heatmap') {
        assert.equal(panel.options.calculate, false);
        assert.equal(panel.targets[0].format, 'heatmap');
        assert.match(panel.targets[0].expr, /sum by \(le\) \(rate\(/);
        assert(!panel.targets[0].expr.includes('average_seconds'));
      }
    }
    if (dashboard.uid !== 'llm-router-telemetry') {
      for (const title of ['Time to first token (mean)', 'Inter-token latency (mean)', 'Engine end-to-end latency (mean)', 'CPU utilization', 'GPU utilization', 'Prefix-cache hit ratio', 'Speculative acceptance']) {
        const panel = charts.find(candidate => candidate.title === title);
        assert(panel, title);
        assert.equal(panel.targets.length, 1);
      }
      const itl = charts.find(panel => panel.title === 'Inter-token latency (mean)');
      assert.equal(itl.fieldConfig.defaults.unit, 'ms');
      assert.match(itl.targets[0].expr, /\* 1000$/);
    }
    if (dashboard.uid === 'llm-platform-overview') {
      assert.deepEqual(dashboard.panels.filter(panel => panel.type === 'row').map(panel => panel.title), ['Health & Reliability', 'Throughput', 'Concurrency & Pressure', 'Latency', 'Payloads', 'Efficiency', 'Infrastructure', 'Token usage']);
      const groups = new Map();
      let current;
      for (const panel of dashboard.panels) {
        if (panel.type === 'row') { current = panel.title; groups.set(current, [...(panel.panels || [])]); }
        else groups.get(current).push(panel);
      }
      assert(groups.get('Health & Reliability').some(panel => panel.title === 'Host sample age'));
      assert(groups.get('Health & Reliability').some(panel => /errors/.test(panel.title)));
      assert(groups.get('Latency').some(panel => panel.title === 'Time to first token (mean)'));
      assert(groups.get('Concurrency & Pressure').some(panel => /preemptions/.test(panel.title)));
      assert(groups.get('Payloads').some(panel => panel.title === 'Router payload traffic'));
      for (const title of ['Router open streams', 'Engine running requests', 'Engine waiting requests']) assert.equal(charts.find(panel => panel.title === title).targets.length, 1);
      const gauges = charts.filter(panel => panel.type === 'gauge');
      assert.equal(gauges.length, 2);
      for (const gauge of gauges) {
        assert.equal(gauge.fieldConfig.defaults.fieldMinMax, true);
        assert.equal(gauge.fieldConfig.defaults.min, 0);
        assert.equal(gauge.fieldConfig.defaults.max, undefined);
        assert.equal(gauge.options.reduceOptions.calcs[0], 'lastNotNull');
        assert.equal(gauge.targets.length, 1);
        assert.match(gauge.targets[0].expr, /^sum by \(model\) \(rate\(/);
        assert.match(gauge.targets[0].expr, /\[1m\]/);
        assert.equal(gauge.targets[0].instant, false);
        assert.equal(gauge.targets[0].range, true);
      }
      assert.match(gauges[0].targets[0].expr, /generation_tokens_total/);
      assert.match(gauges[1].targets[0].expr, /prompt_tokens_total/);
      assert(dashboard.links[0].url.includes('${model:queryparam}'));
      assert(dashboard.links[0].url.includes('${node:queryparam}'));
      assert(dashboard.links[0].url.includes('var-job=${fleet_job:percentencode}'));
      assert(dashboard.links[1].url.includes('${route:queryparam}'));
      assert(dashboard.links.every(link => link.keepTime));
      assert(charts.slice(0, 3).every(panel => panel.gridPos.h === 3 && panel.options.colorMode === 'value'));
    }
    if (dashboard.uid === 'llm-router-telemetry') {
      const repeated = dashboard.panels.filter(panel => panel.type === 'row' && panel.repeat === 'model');
      assert.deepEqual(repeated.map(panel => panel.title), ['Latency - $model', 'Payloads - $model']);
      for (const heatmap of charts.filter(panel => panel.type === 'heatmap')) {
        assert(heatmap.targets[0].expr.includes('model=~"$model"'));
        assert.match(heatmap.targets[0].expr, /and on \(\) \(sum\(increase\(llm_router_\w+_count/);
        assert(heatmap.targets[0].expr.includes('[$__range] @ end()'));
        assert.equal(heatmap.fieldConfig.defaults.noValue, 'No observations');
      }
      const quantiles = charts.filter(panel => /latency percentiles$/.test(panel.title));
      assert.equal(quantiles.length, 2);
      for (const panel of quantiles) {
        assert.match(panel.targets[1].expr, />= 20\)$/);
        assert.match(panel.targets[2].expr, />= 100\)$/);
        assert(panel.targets.every(item => item.expr.includes('[$__rate_interval]')));
      }
      assert.equal(charts.filter(panel => /samples per quantile window/.test(panel.title)).length, 3);
    }
    if (dashboard.uid !== 'vllm-fleet-metrics') {
      const ttft = charts.filter(panel => panel.targets?.some(item => item.expr?.includes('llm_router_time_to_first_byte_seconds')));
      assert(ttft.some(panel => panel.type === 'heatmap'), 'missing time to first byte heatmap');
      const quantiles = ttft.find(panel => panel.title === 'Time to first byte percentiles');
      assert(quantiles, 'missing time to first byte percentiles');
      assert.deepEqual(quantiles.targets.map(item => item.legendFormat), ['{{model}} p50', '{{model}} p95', '{{model}} p99']);
      for (const panel of ttft) assert(panel.targets.every(item => !item.expr.includes('route=')), `${panel.title} filters on route, which the metric does not have`);
    }
    for (const panel of charts) {
      assert(!(panel.transformations || []).some(item => item.id === 'renameByRegex'), 'default charts must retain exact model IDs');
    }
  });
}

test('size buckets match exporter boundaries and canonical numeric labels', () => {
  const exporter = readFileSync(new URL('../metrics.go', import.meta.url), 'utf8');
  const bounds = exporter.match(/sizeBuckets := \[\]float64\{([^}]+)\}/)[1].split(',').map(Number);
  assert.deepEqual(sizeBounds, bounds);
  for (const value of sizeBounds) {
    const regex = new RegExp(`^(?:${boundPattern(value)})$`);
    assert(regex.test(String(value)));
    assert(regex.test(`${value}.0`));
    assert(regex.test(value.toExponential()));
    assert(regex.test(value.toExponential().replace(/e\+(\d)$/, 'e+0$1')));
  }
  assert(new RegExp(`^(?:${boundPattern(Infinity)})$`).test('+Inf'));
  const targets = sizeBucketTargets('size_bucket', 'job="$job",model=~"$model"');
  assert.equal(targets.length, sizeBounds.length + 1);
  assert.equal(targets[0].legendFormat, '0 - 128 B');
  assert.equal(targets.at(-1).legendFormat, '> 32 MiB');
  assert(targets.every(item => item.instant && !item.range && item.expr.includes('[$__range]')));
  assert(targets.slice(1).every(item => item.expr.includes(' - sum(increase(')));
  const cumulative = [1, 3, 7, 7, 11, 12, 12, 12, 13, 13, 13, 14];
  const exclusive = cumulative.map((count, index) => count - (cumulative[index - 1] || 0));
  assert.equal(exclusive.reduce((sum, count) => sum + count, 0), cumulative.at(-1));
});

test('unknown panels and live metadata survive migration', () => {
  const source = JSON.parse(readFileSync(files[0], 'utf8'));
  source.version = 73;
  source.refresh = '37s';
  source.panels.push({ id: 900, title: 'User panel', type: 'text', options: { content: 'keep' }, gridPos: { x: 0, y: 100, w: 24, h: 3 } });
  const result = refineDashboard(source);
  assert.equal(result.version, 73);
  assert.equal(result.refresh, '37s');
  assert(flatten(result.panels).some(panel => panel.id === 900 && panel.options.content === 'keep'));
});

test('unnamed scalar stats never display their PromQL as a name', () => {
  const source = JSON.parse(readFileSync(files[1], 'utf8'));
  const scalar = flatten(source.panels).find(panel => panel.title === 'Routable models');
  delete scalar.targets[0].legendFormat;
  const expression = scalar.targets[0].expr;
  const result = flatten(refineDashboard(source).panels).find(panel => panel.title === scalar.title);
  assert.equal(result.targets[0].expr, expression);
  assert.equal(result.targets[0].legendFormat, 'Routable models');
  assert.equal(result.fieldConfig.defaults.displayName, 'Routable models');
  assert.equal(result.options.textMode, 'value');
});

function exporterMetrics() {
  const source = readFileSync(new URL('../metrics.go', import.meta.url), 'utf8');
  const shared = source.match(/labels := \[\]string\{([^}]*)\}/)[1];
  const parse = list => list.match(/"([^"]+)"/g)?.map(item => item.slice(1, -1)) || [];
  const metrics = new Map();
  for (const [, kind, name, labels] of source.matchAll(/prometheus\.New(Counter|Gauge|Histogram)Vec\(prometheus\.\w+Opts\{Name: "(\w+)".*?\}, (labels|\[\]string\{[^}]*\})\)/g)) {
    const set = new Set(parse(labels === 'labels' ? shared : labels));
    metrics.set(name, set);
    if (kind === 'Histogram') {
      metrics.set(`${name}_bucket`, new Set([...set, 'le']));
      metrics.set(`${name}_count`, set);
      metrics.set(`${name}_sum`, set);
    }
  }
  for (const [, name, labels] of source.matchAll(/prometheus\.NewDesc\("(\w+)", "[^"]*", (nil|\[\]string\{[^}]*\})/g)) {
    metrics.set(name, new Set(labels === 'nil' ? [] : parse(labels)));
  }
  return metrics;
}

test('dashboards query only router metrics and labels that the exporter has', () => {
  const metrics = exporterMetrics();
  assert(metrics.has('llm_router_time_to_first_byte_seconds_bucket'));
  assert(metrics.get('llm_router_requests_total').has('outcome'));
  assert(!metrics.get('llm_router_requests_total').has('result'));
  const generic = new Set(['job', 'instance', '__name__']);
  for (const file of files) {
    const dashboard = JSON.parse(readFileSync(file, 'utf8'));
    const queries = [
      ...flatten(dashboard.panels).flatMap(panel => (panel.targets || []).filter(item => item.expr).map(item => ({ where: panel.title, expr: item.expr, legend: item.legendFormat || '' }))),
      ...dashboard.templating.list.filter(variable => typeof (variable.query?.query ?? variable.query) === 'string').map(variable => ({ where: `$${variable.name}`, expr: variable.query?.query ?? variable.query, legend: '' })),
    ];
    for (const { where, expr, legend } of queries) {
      const names = [...expr.matchAll(/\b(llm_router_\w+)/g)].map(match => match[1]);
      for (const name of names) assert(metrics.has(name), `${where}: unknown metric ${name}`);
      for (const [, name, selector] of expr.matchAll(/\b(llm_router_\w+)\{([^}]*)\}/g)) {
        for (const [, label] of selector.matchAll(/(\w+)\s*(?:=~|!~|!=|=)/g)) {
          assert(generic.has(label) || metrics.get(name).has(label), `${where}: ${name} has no label ${label}`);
        }
      }
      if (!names.length || /vllm_dashboard_/.test(expr)) continue;
      const known = new Set([...generic, ...names.flatMap(name => [...metrics.get(name)])]);
      const grouped = [...expr.matchAll(/\b(?:by|on|ignoring|without)\s*\(([^)]*)\)/g)].flatMap(match => match[1].split(',').map(item => item.trim()).filter(Boolean));
      const shown = [...legend.matchAll(/\{\{\s*(\w+)\s*\}\}/g)].map(match => match[1]);
      const variable = expr.match(/label_values\([^,]+,\s*(\w+)\)/)?.[1];
      for (const label of [...grouped, ...shown, ...(variable ? [variable] : [])]) {
        assert(known.has(label), `${where}: label ${label} is not on ${names.join(', ')}`);
      }
    }
  }
});
