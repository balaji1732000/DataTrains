import type { Metadata } from "next";
import "./styles.css";

export const metadata: Metadata = {
  title: "DataTrains Control Plane",
  description: "Professional computer-use data collection and review",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
