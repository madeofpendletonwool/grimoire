// Indirection for "open this reference", so rendering code can emit clickable
// rule and card chips without importing the drawer (which imports rendering).
// The drawer registers the real handlers at start-up.

export const refs = {
	openRule: () => {},
	openCard: () => {},
};

export function registerRefHandlers(handlers) {
	Object.assign(refs, handlers);
}
