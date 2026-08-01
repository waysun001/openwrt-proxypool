(function(){
  'use strict';
  function ready(fn){if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',fn);else fn();}
  function build(){
    if(document.getElementById('proxypool-global-menu'))return;
    var menu=document.createElement('div');menu.id='proxypool-global-menu';
    var links=document.createElement('div');links.className='menu-links';
    [['ProxyPool','/cgi-bin/luci/admin/services/proxypool'],['备份与升级','/cgi-bin/luci/admin/system/flash'],['无线','/cgi-bin/luci/admin/network/wireless'],['重启','/cgi-bin/luci/admin/system/reboot']].forEach(function(item){var a=document.createElement('a');a.href=item[1];a.textContent=item[0];if(location.pathname.replace(/\/+$/,'')===item[1])a.className='active';links.appendChild(a);});
    var stats=document.createElement('div');stats.className='menu-stats';stats.innerHTML='<span>节点 <strong id="pp-stat-total">-</strong></span><span class="stat-connected">在线 <strong id="pp-stat-connected">-</strong></span><span class="stat-disconnected">离线 <strong id="pp-stat-disconnected">-</strong></span>';
    menu.appendChild(links);menu.appendChild(stats);var anchor=document.querySelector('#maincontent')||document.querySelector('.main-right')||document.querySelector('.main')||document.querySelector('#content');if(anchor&&anchor.parentNode)anchor.parentNode.insertBefore(menu,anchor);else document.body.insertBefore(menu,document.body.firstChild);
    document.title='ProxyPool';
    fetch('/cgi-bin/luci/admin/services/proxypool/api?action=status',{credentials:'same-origin'}).then(function(r){return r.json();}).then(function(envelope){var data=envelope&&envelope.result;if(!envelope.success||!data)return;var nodes=data.runtime.nodes||[],online=nodes.filter(function(n){return n.state==='online';}).length;document.getElementById('pp-stat-total').textContent=nodes.length;document.getElementById('pp-stat-connected').textContent=online;document.getElementById('pp-stat-disconnected').textContent=nodes.length-online;}).catch(function(){});
  }
  ready(build);
})();
