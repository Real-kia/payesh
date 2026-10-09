// Development builds use the real API by default. Opt into fixtures
// explicitly so a local preview can never accidentally mask API failures.
// (import.meta.env only exists in Vite builds; node unit tests import this too.)
export const PREVIEW_MODE = import.meta.env?.VITE_PAYESH_PREVIEW === 'true';
