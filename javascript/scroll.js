new Promise(resolve => {
    const root = document.scrollingElement || document.documentElement;
    if (!root) {
        resolve();
        return;
    }
    const started = Date.now();
    let lastHeight = root.scrollHeight;
    let stableTicks = 0;
    const timer = setInterval(() => {
        const distance = Math.max(root.clientHeight, window.innerHeight, 400);
        root.scrollBy(0, distance);
        const height = root.scrollHeight;
        const atBottom = root.scrollTop + root.clientHeight >= height - 2;
        stableTicks = atBottom && height === lastHeight ? stableTicks + 1 : 0;
        lastHeight = height;
        if (stableTicks >= 2 || Date.now() - started >= 10000) {
            clearInterval(timer);
            root.scrollTo(0, root.scrollHeight);
            resolve();
        }
    }, 200);
})
