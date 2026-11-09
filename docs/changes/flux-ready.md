### Changed

- The add-on room check counts the memory the add-ons' pods request as well as use, and refuses a worker that holds only the use, as their pods would stay pending.
