import assert from "node:assert/strict";
import test from "node:test";
import {readFileSync} from "node:fs";
import vm from "node:vm";
import * as household from "../frontend/household.mjs";
import * as metrics from "../frontend/metrics.mjs";
import {setupUpdates, waitForUpdateRestart} from "../frontend/updates.mjs";

test("waits through the old service and disconnect until the expected version responds", async () => {
  const replies = [
    {version:"0.0.7-alpha.1",state:"active"},
    new Error("service disconnected"),
    {state:"active"},
    {version:"0.0.7-alpha.2",state:"paused"},
  ];
  const outcome = await waitForUpdateRestart("v0.0.7-alpha.2", async () => {
    const reply = replies.shift();
    if (reply instanceof Error) throw reply;
    return reply;
  }, {timeoutMS:1000,intervalMS:1});
  assert.equal(outcome.state, "ready");
  assert.equal(outcome.status.version, "0.0.7-alpha.2");
  assert.equal(replies.length, 0);
});

test("an old version after rollback never counts as update completion", async () => {
  const outcome = await waitForUpdateRestart("0.0.7-alpha.2", async () => ({version:"0.0.7-alpha.1"}), {timeoutMS:20,intervalMS:1});
  assert.equal(outcome.state, "timeout");
  assert.equal(outcome.status.version, "0.0.7-alpha.1");
});

test("a service that stays unavailable reaches the recovery state", async () => {
  const outcome = await waitForUpdateRestart("0.0.7-alpha.2", async () => { throw new Error("unavailable"); }, {timeoutMS:20,intervalMS:1});
  assert.deepEqual(outcome, {state:"timeout",status:null});
});

test("a hung IPC request cannot defeat the deadline or start overlapping polls", async () => {
  let calls = 0;
  let respond;
  const outcome = await waitForUpdateRestart("0.0.7-alpha.2", () => {
    calls++;
    return new Promise((resolve) => { respond = resolve; });
  }, {timeoutMS:20,intervalMS:1});
  assert.deepEqual(outcome, {state:"timeout",status:null});
  respond({version:"0.0.7-alpha.2"});
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(calls, 1);
  assert.equal(outcome.state, "timeout");
});

test("missing target and service versions never count as success", async () => {
  const outcome = await waitForUpdateRestart("", async () => ({}), {timeoutMS:20,intervalMS:1});
  assert.equal(outcome.state, "timeout");
});

function updateWindow(readStatus, timeoutMS) {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) {
      const classes = new Set();
      nodes.set(id, {
        dataset:{},value:"",disabled:false,textContent:"",firstChild:{},listeners:{},
        classList:{add:(c)=>classes.add(c),remove:(c)=>classes.delete(c),contains:(c)=>classes.has(c),toggle:(c,on)=>on?classes.add(c):classes.delete(c)},
        setAttribute(){},
        addEventListener(event, callback) { this.listeners[event] = callback; },
      });
    }
    return nodes.get(id);
  };
  const context = vm.createContext({
    ...metrics,
    ...household,
    setupUpdates:(options)=>setupUpdates({...options,waitForRestart:(version,read)=>waitForUpdateRestart(version,read,{timeoutMS,intervalMS:1})}),
    document:{hidden:true,documentElement:{},getElementById:node,querySelector:node,querySelectorAll:()=>[],addEventListener(){},
      createElement:()=>{const el={};Object.defineProperty(el,"textContent",{set(v){el.innerHTML=String(v).replace(/[&<>"]/g,(c)=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));}});return el;}},
    console,
    window:{addEventListener(){},wails:{Call:{ByName:async(name)=>{
      if (name.endsWith(".CheckUpdate")) return {available_version:"0.0.7-alpha.2"};
      if (name.endsWith(".StageUpdate")) return {restarting:true,available_version:"0.0.7-alpha.2"};
      if (name.endsWith(".Status")) return readStatus();
      throw new Error(`Unexpected call: ${name}`);
    }}}},
    localStorage:{getItem:()=>null},
    setTimeout:(callback,delay)=>setTimeout(callback,delay).unref(),
  });
  const source = readFileSync(new URL("../frontend/app.js",import.meta.url),"utf8").replace(/^import .*;\n/gm,"");
  vm.runInContext(source,context);
  node.context = context;
  return node;
}

for (const succeeds of [true,false]) {
  test(`update button restores controls and replaces restarting text (${succeeds?"success":"rollback"})`, async()=>{
    let calls=0;
    let polled;
    const firstPoll=new Promise((resolve)=>{polled=resolve;});
    const node=updateWindow(()=>{polled();return {state:"active",mode:"standalone",version:++calls>1&&succeeds?"0.0.7-alpha.2":"0.0.7-alpha.1"};},succeeds?1000:30);
    const button=node("update-button");
    await button.listeners.click();
    const pending=button.listeners.click();
    await firstPoll;
    assert.equal(button.classList.contains("hidden"),false);
    assert.equal(button.disabled,true);
    assert.equal(button.textContent,"Restarting service…");
    assert.equal(node("update-stream").disabled,true);
    assert.match(node("update-status").textContent,/restarting/);
    await pending;
    assert.equal(button.disabled,false);
    assert.equal(button.classList.contains("hidden"),false);
    assert.equal(button.textContent,"Check");
    assert.equal(node("update-stream").disabled,false);
    assert.match(node("update-status").textContent,succeeds?/0\.0\.7-alpha\.2 is installed/:/did not finish.*0\.0\.7-alpha\.1/);
    assert.doesNotMatch(node("toast").textContent,/not defined|not a function/);
  });
}

test("a config version change in a live tick refetches the full status and rebuilds the picker", async()=>{
  let statusCalls=0;
  const node=updateWindow(()=>{statusCalls++;return {state:"active",mode:"connected",version:"0.0.7-alpha.5",config_version:9,speed_profiles:[{name:"content"},{name:"household"},{name:"saturation"}]};},1000);
  const {render}=node.context;
  render({state:"active",config_version:8,version:"0.0.7-alpha.5"});
  render({state:"active",config_version:8,version:"0.0.7-alpha.5"});
  await new Promise((resolve)=>setTimeout(resolve,5));
  assert.equal(statusCalls,0,"an unchanged version must not refetch");
  render({state:"active",config_version:9,version:"0.0.7-alpha.5"});
  await new Promise((resolve)=>setTimeout(resolve,20));
  assert.equal(statusCalls,1);
  assert.match(node("speed-profile").innerHTML,/household/);
  assert.match(node("speed-profile").innerHTML,/saturation/);
  render({state:"active",config_version:9,version:"0.0.7-alpha.5"});
  await new Promise((resolve)=>setTimeout(resolve,5));
  assert.equal(statusCalls,1,"the refetched status must not trigger another refetch");
});
