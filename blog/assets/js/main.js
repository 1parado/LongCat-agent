document.addEventListener('DOMContentLoaded', () => {
  const rows = document.querySelectorAll('.article-row')
  rows.forEach((row, index) => {
    row.style.setProperty('--row-delay', `${index * 55}ms`)
    row.classList.add('ready')
  })
})
