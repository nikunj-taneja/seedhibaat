// The tooltip lives once at the end of the SVG so it paints above every
// bar. Without JS the native <title> on each group still works.
(function () {
  var tip = document.getElementById('chart-tip');
  if (!tip) { return; }
  var box = tip.querySelector('.chart-tip-box');
  var title = tip.querySelector('.chart-tip-title');
  var detail = tip.querySelector('.chart-tip-detail');
  var boxWidth = 150, boxHeight = 38, plotLeft = 50, plotRight = 930;

  function show(group) {
    title.textContent = group.dataset.label;
    detail.textContent = group.dataset.delivered + ' delivered · ' + group.dataset.clicks + ' clickers';
    var center = Number(group.dataset.center);
    var top = Number(group.dataset.top);
    var x = Math.min(Math.max(center - boxWidth / 2, plotLeft), plotRight - boxWidth);
    var y = top - boxHeight - 6;
    if (y < 2) { y = top + 6; }
    tip.setAttribute('transform', 'translate(' + x + ',' + y + ')');
    box.setAttribute('width', boxWidth);
    box.setAttribute('height', boxHeight);
    tip.removeAttribute('hidden');
  }

  function hide() { tip.setAttribute('hidden', ''); }

  Array.prototype.forEach.call(document.querySelectorAll('.chart-day'), function (group) {
    group.addEventListener('mouseenter', function () { show(group); });
    group.addEventListener('focus', function () { show(group); });
    group.addEventListener('mouseleave', hide);
    group.addEventListener('blur', hide);
  });
})();
