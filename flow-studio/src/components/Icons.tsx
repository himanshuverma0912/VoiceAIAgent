// Line icons for the sidebar, chips and buttons (24×24, drawn with currentColor).
import type { ReactNode } from "react";

function Svg({ children, size = 20 }: { children: ReactNode; size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {children}
    </svg>
  );
}

type P = { size?: number };

/** Flows: a sparkle with a small flow underneath. */
export const IconStudio = ({ size }: P) => (
  <Svg size={size}>
    <path d="M10 3.5c.5 2.9 1.6 4 4.5 4.5-2.9.5-4 1.6-4.5 4.5-.5-2.9-1.6-4-4.5-4.5 2.9-.5 4-1.6 4.5-4.5Z" />
    <path d="M17.5 12.5c.3 1.6.9 2.2 2.5 2.5-1.6.3-2.2.9-2.5 2.5-.3-1.6-.9-2.2-2.5-2.5 1.6-.3 2.2-.9 2.5-2.5Z" />
    <circle cx="6" cy="18" r="2.2" />
    <path d="M8.2 18H12" />
  </Svg>
);

export const IconReviews = ({ size }: P) => (
  <Svg size={size}>
    <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h4.5" />
    <path d="M14 3l5 5h-5V3Z" />
    <path d="M9 12h6M9 15.5h3" />
    <circle cx="17" cy="17.5" r="3.5" />
    <path d="m15.6 17.5 1 1 1.9-1.9" />
  </Svg>
);

export const IconWorkspaces = ({ size }: P) => (
  <Svg size={size}>
    <rect x="3.5" y="3.5" width="7" height="7" rx="2" />
    <rect x="13.5" y="3.5" width="7" height="7" rx="2" />
    <rect x="3.5" y="13.5" width="7" height="7" rx="2" />
    <rect x="13.5" y="13.5" width="7" height="7" rx="2" />
  </Svg>
);

export const IconModels = ({ size }: P) => (
  <Svg size={size}>
    <rect x="6" y="6" width="12" height="12" rx="2.5" />
    <rect x="9.5" y="9.5" width="5" height="5" rx="1" />
    <path d="M9 3v3M15 3v3M9 18v3M15 18v3M3 9h3M3 15h3M18 9h3M18 15h3" />
  </Svg>
);

export const IconIntegrations = ({ size }: P) => (
  <Svg size={size}>
    <path d="M9 3v4M15 3v4" />
    <path d="M6.5 7h11v3.5a5.5 5.5 0 0 1-11 0V7Z" />
    <path d="M12 16v5" />
  </Svg>
);

export const IconChannel = ({ size }: P) => (
  <Svg size={size}>
    <rect x="3.5" y="3.5" width="17" height="17" rx="4" />
    <circle cx="8.5" cy="9" r="1.8" />
    <circle cx="15.5" cy="9" r="1.8" />
    <circle cx="12" cy="15.5" r="1.8" />
    <path d="M9.6 10.4l1.5 3.6M14.4 10.4l-1.5 3.6" />
  </Svg>
);

export const IconSignOut = ({ size }: P) => (
  <Svg size={size}>
    <path d="M14 4h3.5A2.5 2.5 0 0 1 20 6.5v11a2.5 2.5 0 0 1-2.5 2.5H14" />
    <path d="M10 8l-4 4 4 4M6 12h10" />
  </Svg>
);

export const IconSearch = ({ size }: P) => (
  <Svg size={size}>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m16 16 4 4" />
  </Svg>
);

export const IconPhone = ({ size }: P) => (
  <Svg size={size}>
    <path d="M7.5 3.5h-2a2 2 0 0 0-2 2.2c.9 7.6 7.2 13.9 14.8 14.8a2 2 0 0 0 2.2-2v-2l-4.2-1.8-2 2a12 12 0 0 1-5.5-5.5l2-2L9.3 3.5H7.5Z" />
  </Svg>
);

export const IconVersion = ({ size }: P) => (
  <Svg size={size}>
    <circle cx="6.5" cy="6" r="2.2" />
    <circle cx="6.5" cy="18" r="2.2" />
    <circle cx="17.5" cy="9" r="2.2" />
    <path d="M6.5 8.2v7.6M17.5 11.2c0 3-3 3.3-6 3.8-2 .3-3.6 1-4.2 1.6" />
  </Svg>
);

export const IconClock = ({ size }: P) => (
  <Svg size={size}>
    <circle cx="12" cy="12" r="8.5" />
    <path d="M12 7.5V12l3 2" />
  </Svg>
);

export const IconCheck = ({ size }: P) => (
  <Svg size={size}>
    <path d="m6 12.5 4 4 8-9" />
  </Svg>
);

export const IconCross = ({ size }: P) => (
  <Svg size={size}>
    <path d="M7 7l10 10M17 7 7 17" />
  </Svg>
);

export const IconDraft = ({ size }: P) => (
  <Svg size={size}>
    <path d="M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16v4Z" />
    <path d="m13.5 6.5 4 4" />
  </Svg>
);

export const IconStack = ({ size }: P) => (
  <Svg size={size}>
    <path d="M12 3.5 3.5 8 12 12.5 20.5 8 12 3.5Z" />
    <path d="M3.5 12 12 16.5 20.5 12M3.5 16 12 20.5 20.5 16" />
  </Svg>
);

export const IconSun = ({ size }: P) => (
  <Svg size={size}>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4" />
  </Svg>
);

export const IconMoon = ({ size }: P) => (
  <Svg size={size}>
    <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z" />
  </Svg>
);

export const IconChevron = ({ size }: P) => (
  <Svg size={size}>
    <path d="m15 6-6 6 6 6" />
  </Svg>
);
